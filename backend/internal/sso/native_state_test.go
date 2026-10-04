//go:build linux

package sso

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeUserStorageAdmission(t *testing.T) {
	for _, damage := range []string{"none", "inactive", "hold", "missing-state", "missing-mailbox", "missing-ledger", "foreign-owner", "source", "root"} {
		t.Run(damage, func(t *testing.T) {
			config, root := t.TempDir(), t.TempDir()
			life := NewLifecycleStore(config)
			domains := provenNativeDomain(t, config)
			accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
			if err != nil {
				t.Fatal(err)
			}
			nativeDesired(t, life, "one", "one@example.test", 1, true)
			u, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", domains, accounts, nativeLimits)
			if err != nil {
				t.Fatal(err)
			}
			absent := ""
			switch damage {
			case "inactive":
				u.Active = false
			case "hold":
				err = os.WriteFile(filepath.Join(root, NativeRestoreHoldFile), []byte("malformed"), 0600)
			case "missing-state":
				absent = filepath.Join(root, "users", u.ID, "state.db")
				err = os.Remove(absent)
			case "missing-mailbox":
				absent = filepath.Join(root, "users", u.ID, "mailbox", "mailbox.db")
				err = os.Remove(absent)
			case "missing-ledger":
				err = os.Remove(life.nativePath())
			case "foreign-owner":
				u.SSOSub = "foreign"
			case "source":
				u.NativeMailboxSource = "native:foreign"
			case "root":
				root = t.TempDir()
			}
			if err != nil {
				t.Fatal(err)
			}
			ownershipErr := life.ValidateNativeUserOwnership(root, u)
			if (ownershipErr == nil) != (damage == "none" || damage == "inactive" || damage == "hold") {
				t.Fatalf("revocation ownership accepted=%t error=%v", ownershipErr == nil, ownershipErr)
			}
			err = life.ValidateNativeUserStorage(root, u)
			good := damage == "none" || damage == "inactive"
			if (err == nil) != good {
				t.Fatalf("accepted=%t error=%v", err == nil, err)
			}
			if absent != "" {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("recreated missing storage: %v", err)
				}
			}
		})
	}
	// A restore hold affects native mail only; legacy accounts stay on their path.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, NativeRestoreHoldFile), []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewLifecycleStore(t.TempDir()).ValidateNativeUserStorage(root, users.User{ID: "legacy"}); err != nil {
		t.Fatal(err)
	}
}
