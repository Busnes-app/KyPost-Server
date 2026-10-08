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
	"slices"
	"strings"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func nativeSnapshot(dir string) (bool, error) {
	// Snapshots may predate migration, so domain and relay accept both formats.
	relay, relayVersion, err := mailmsg.ReadDomainRelayAnyVersion(filepath.Join(dir, "config/native-relay.json"), filepath.Join(dir, "private/native-relay.key"))
	if err != nil {
		return true, err
	}
	hasRelay := relayVersion != 0
	if hasRelay {
		// Live relay domains are configured, never retired; retired relay
		// domains stay within the domain set's history.
		domains, domainFormat, err := sso.HistoricalNativeDomains(filepath.Join(dir, "config"))
		configured := func(d string) bool { _, ok := domains.Domains[d]; return ok }
		if err != nil || !sso.NativeSnapshotFormatsConsistent(domainFormat, relayVersion) || domains.Issuer != relay.Issuer || !every(relay.Domains, configured) || !every(relay.RetiredDomains, domains.Known) {
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

func every(values []string, ok func(string) bool) bool {
	return !slices.ContainsFunc(values, func(v string) bool { return !ok(v) })
}

// Validate the collected bytes, not live files that can change during collection.
// staged maps capsule paths of bulk mail snapshots to their files on disk.
func validateNativePayload(ctx context.Context, files []recoveryclient.File, staged map[string]string, scratch string) error {
	native := len(staged) > 0
	for _, f := range files {
		switch filepath.Base(f.Path) {
		case ingress.BlocksFile:
			if _, err := ingress.ParseBlocks(f.Data); err != nil {
				return fmt.Errorf("refusing to seal %s: %w", f.Path, err)
			}
		case "native-provisioning.json", sso.NativeDomainsFile, "native-mailbox.json", "mailbox.db", "ingress.db", "native-relay.json":
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
		if base != "users.json" && base != "sso-lifecycle.json" && base != "native-domain.json" && base != sso.NativeDomainsFile && base != "native-provisioning.json" && base != "native-mailbox.json" && base != "native-relay.json" && base != "native-relay.key" && !snapshotDatabase(base) {
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
	// Validators open databases read-only, so a link stands in for a multi-GB copy.
	for rel, src := range staged {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.Link(src, path); err != nil {
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
	// Persist an unusable epoch first: crypto/rand may terminate the process.
	// No outstanding reconciliation survives a failed generation attempt.
	hold := map[string]any{"version": 1, "epoch": "", "reason": "restore_requires_identity_domain_receiver_reconciliation"}
	// The qualification marker exists only once this run's stages all succeed.
	if err := os.Remove(filepath.Join(dir, "state", sso.NativeRestoreQualificationFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	holdErr := fsutil.PersistJSONFile(path, hold)
	if holdErr != nil {
		return true, fmt.Errorf("cannot persist native restore hold; keep workers stopped: %w", holdErr)
	}
	epoch, epochErr := fsutil.NewUUIDv4()
	if epochErr != nil {
		return true, fmt.Errorf("cannot create native restore epoch; preserve held staging and keep workers stopped: %w", epochErr)
	}
	hold["epoch"] = epoch
	if err := fsutil.PersistJSONFile(path, hold); err != nil {
		return true, fmt.Errorf("cannot persist native restore epoch; preserve held staging and keep workers stopped: %w", err)
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
	if err := fenceRestoredNativeAccounts(dir); err != nil {
		return true, fmt.Errorf("cannot fence restored native references/credentials; keep workers stopped and preserve staging: %w", err)
	}
	if err := sso.NewLifecycleStore(filepath.Join(dir, "config")).RecordNativeRestoreQualification(filepath.Join(dir, "state")); err != nil {
		return true, fmt.Errorf("cannot record native restore qualification; keep workers stopped and preserve staging: %w", err)
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

// fenceRestoredNativeAccounts selects only historically qualified native users.
// The whole restore stays held and unpublished if any account mutation fails.
func fenceRestoredNativeAccounts(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "config/users.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // Whole-snapshot validation already refused missing native users.
	}
	if err != nil {
		return err
	}
	var doc struct {
		Users []users.User `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	for _, u := range doc.Users {
		if u.NativeMailboxSource == "" {
			continue
		}
		if !fsutil.SafePathComponent(u.ID) {
			return sso.ErrNativeProvisioning
		}
		if err := mailbox.RotateRestoredMessageReferences(filepath.Join(dir, "state/users", u.ID, "mailbox/mailbox.db"), u.NativeMailboxSource); err != nil {
			return err
		}
		if err := revokeRestoredDeviceCredentials(filepath.Join(dir, "state/users", u.ID, "state.db"), u.NativeMailboxSource); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, "config/users", u.ID, "carddav-auth.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	life := sso.NewLifecycleStore(filepath.Join(dir, "config"))
	// Extra mailboxes hold mail only: rotate their references; devices and
	// CardDAV were revoked with their owner above.
	extras, err := life.NativeExtraMailboxSources()
	if err != nil {
		return err
	}
	for id, source := range extras {
		if err := mailbox.RotateRestoredMessageReferences(filepath.Join(dir, "state/mailboxes", id, "mailbox/mailbox.db"), source); err != nil {
			return err
		}
	}
	return life.FenceRestoredNativeTokens(filepath.Join(dir, "state"), doc.Users)
}

// Revoke notification targets and outstanding stateless pairing tokens together.
// Fresh pairing is required after recovery; mail and wrapped key material stay
// intact. No schema initialization/migration or native-to-IMAP adoption occurs.
func revokeRestoredDeviceCredentials(path, source string) (err error) {
	if !strings.HasPrefix(source, "native:") {
		return state.ErrMailSource
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	location := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open("sqlite", location.String()+"?mode=rw&_txlock=immediate&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var storedSource string
	if err := tx.QueryRow("SELECT value FROM meta WHERE key='mail_source'").Scan(&storedSource); err != nil {
		return err
	}
	if storedSource != source {
		return state.ErrMailSource
	}
	subscriber, err := fsutil.NewUUIDv4()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM native_devices; DELETE FROM notifications"); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES('subscriber_id',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", subscriber); err != nil {
		return err
	}
	return tx.Commit()
}
