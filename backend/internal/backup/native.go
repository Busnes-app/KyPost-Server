package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func nativeSnapshot(dir string) (bool, error) {
	relay, hasRelay, err := mailmsg.ReadDomainRelay(filepath.Join(dir, "config/native-relay.json"), filepath.Join(dir, "private/native-relay.key"))
	if err != nil {
		return true, err
	}
	if hasRelay {
		domain, err := sso.NewNativeDomainStore(filepath.Join(dir, "config")).Read()
		if err != nil || domain.Domain != relay.Domain || domain.Issuer != relay.Issuer {
			return true, mailmsg.ErrDomainRelay
		}
	}
	var doc struct {
		Users []users.User `json:"users"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config/users.json"))
	usersErr := err
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return true, err
		}
	}
	native, err := sso.NewLifecycleStore(filepath.Join(dir, "config")).ValidateNativeSnapshot(filepath.Join(dir, "state"), doc.Users)
	if native && usersErr != nil {
		return true, usersErr
	}
	if err != nil {
		return true, err
	}
	qualified := native
	native = native || hasRelay
	err = filepath.WalkDir(filepath.Join(dir, "state"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && snapshotDatabase(entry.Name()) {
			if entry.Name() != "state.db" {
				native = true
				if !qualified {
					return sso.ErrNativeProvisioning
				}
			}
			if !integrityOK(path) {
				return errors.New("restored SQLite integrity check failed; preserve staging files")
			}
			if entry.Name() == "mailbox.db" {
				key, _ := cryptutil.LoadKey(filepath.Join(dir, "private/native-relay.key"))
				if _, err := mailbox.ValidateOutboundSnapshot(context.Background(), path, key, relay); err != nil {
					return err
				}
			}

		}
		return nil
	})
	return native, err
}

// Validate the collected bytes, not live files that can change during collection.
func validateNativePayload(ctx context.Context, files []recoveryclient.File, scratch string) error {
	native := false
	for _, f := range files {
		switch filepath.Base(f.Path) {
		case "native-provisioning.json", "native-mailbox.json", "mailbox.db", "ingress.db", "native-relay.json":
			native = true
		case "sso-lifecycle.json":
			var doc struct {
				Initialized bool `json:"nativeProvisioningInitialized"`
			}
			if err := json.Unmarshal(f.Data, &doc); err != nil {
				return err
			}
			native = native || doc.Initialized
		case "users.json":
			var doc struct {
				Users []users.User `json:"users"`
			}
			if err := json.Unmarshal(f.Data, &doc); err != nil {
				return err
			}
			for _, u := range doc.Users {
				native = native || u.NativeMailboxIssuer != "" || u.NativeMailboxSource != ""
			}
		}
	}
	if !native {
		return nil
	}
	dir, err := os.MkdirTemp(scratch, "native-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Validate ownership metadata, relay ciphertext/key and database snapshots.
		base := filepath.Base(f.Path)
		if base != "users.json" && base != "sso-lifecycle.json" && base != "native-domain.json" && base != "native-provisioning.json" && base != "native-mailbox.json" && base != "native-relay.json" && base != "native-relay.key" && !snapshotDatabase(base) {
			continue
		}
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Data, 0600); err != nil {
			return err
		}
	}
	_, err = nativeSnapshot(dir)
	if err != nil {
		return fmt.Errorf("native backup ownership check failed; stop services and reconcile storage before exporting: %w", err)
	}
	return nil
}

// QuarantineNativeRestore is post-decryption product validation. It never
// opens a capsule or handles shares. Leave all restored data intact on failure.
func QuarantineNativeRestore(dir string) (bool, error) {
	path := filepath.Join(dir, "state", sso.NativeRestoreHoldFile)
	_, priorErr := os.Lstat(path)
	// A hold persists even when ownership validation fails. Fresh external
	// reconciliation, not old backup evidence, must authorize its future release.
	holdErr := fsutil.PersistJSONFile(path, map[string]any{"version": 1, "reason": "restore_requires_identity_domain_receiver_reconciliation"})
	if holdErr != nil {
		return true, fmt.Errorf("cannot persist native restore hold; keep workers stopped: %w", holdErr)
	}
	native, checkErr := nativeSnapshot(dir)
	if !native && checkErr == nil && errors.Is(priorErr, os.ErrNotExist) {
		if err := os.Remove(path); err != nil {
			return true, err
		}
		return false, fsutil.SyncDir(filepath.Dir(path))
	}
	if checkErr != nil {
		return true, fmt.Errorf("restored native data is unqualified; keep workers stopped and preserve the staging files for reconciliation: %w", checkErr)
	}
	// Validation above is read-only. Only now mutate private, stopped staging;
	// restored queue evidence cannot authorize a future provider submission.
	if err := filepath.WalkDir(filepath.Join(dir, "state"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "mailbox.db" {
			return quarantineRestoredOutbox(path)
		}
		return nil
	}); err != nil {
		return true, fmt.Errorf("cannot quarantine restored outgoing work; keep workers stopped and preserve staging: %w", err)
	}
	return true, nil
}

// quarantineRestoredOutbox runs only after whole-snapshot qualification and hold
// persistence. It preserves ciphertext, claims, accepted/Sent and ambiguous
// evidence. Each database commits atomically; partial multi-store failure keeps
// the whole restore unpublished and held, and retry is idempotent.
func quarantineRestoredOutbox(path string) (err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	location := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open("sqlite", location.String()+"?mode=rw&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('outbox','outbox_deliveries')").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	if tables != 2 {
		return mailbox.ErrOutbound
	}
	_, err = db.Exec("UPDATE outbox_deliveries SET state='quarantined',next_attempt=0 WHERE state IN ('queued','retryable')")
	return err
}
