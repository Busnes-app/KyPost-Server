package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
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
	return true, nil
}
