//go:build linux

package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func publishedRecoveryFixture(t *testing.T, active bool) (*LifecycleStore, string, SSOSettings, []byte, *users.Store, users.User) {
	t.Helper()
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
	u, err = accounts.SetRole(u.ID, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err = fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	settings := SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}
	key := []byte(strings.Repeat("k", 32))
	fingerprint := sha256.Sum256(key)
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
		c, err := life.BeginNativeRecoveryHeld(context.Background(), root, settings, key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
		if err != nil {
			return err
		}
		body, headers := recoveryPayload(t, c, key, func(e *nativeRecoveryEvidence) {
			revision := int64(1) // Live expiry/offboarding need not allocate a revision.
			e.Subjects[0].ID = "one"
			e.Subjects[0].Revision = &revision
			if active {
				e.Subjects[0].Profile = json.RawMessage(`{"id":"one","externalId":"one","userName":"one","active":true,"roles":[]}`)
			} else {
				e.Subjects[0].Profile = json.RawMessage(`{"id":"one","externalId":"one","active":false,"roles":[]}`)
			}
		})
		return life.AcceptNativeRecoveryHeld(context.Background(), root, settings, key, all, body, headers)
	})
	if err != nil {
		t.Fatal(err)
	}
	return life, root, settings, key, accounts, u
}

func TestNativeRecoveryRepairPlanUsesUnchangedRevisionAuthority(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "inactive"
		if active {
			name = "demoted"
		}
		t.Run(name, func(t *testing.T) {
			life, root, settings, key, accounts, u := publishedRecoveryFixture(t, active)
			before, _ := accounts.List()
			lifecycle, _ := os.ReadFile(life.path)
			hold, _ := os.ReadFile(filepath.Join(root, NativeRestoreHoldFile))
			release, err := life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			err = accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
				plan, err := life.NativeRecoveryRepairPlanHeld(context.Background(), root, settings, key, all)
				if err != nil {
					return err
				}
				want := []users.NativeAccountRepair{{ID: u.ID, Subject: u.SSOSub, Source: u.NativeMailboxSource, Active: active, Role: users.RoleUser}}
				if !reflect.DeepEqual(plan, want) {
					t.Fatal("wrong native access plan")
				}
				return nil
			})
			release()
			if err != nil {
				t.Fatal(err)
			}
			after, _ := accounts.List()
			currentLifecycle, _ := os.ReadFile(life.path)
			currentHold, _ := os.ReadFile(filepath.Join(root, NativeRestoreHoldFile))
			if !reflect.DeepEqual(before, after) || !bytes.Equal(lifecycle, currentLifecycle) || !bytes.Equal(hold, currentHold) {
				t.Fatal("planning mutated retained authority or released hold")
			}
		})
	}
}

func TestNativeRecoveryRepairPlanRechecksReceiptAndStorage(t *testing.T) {
	for _, damage := range []string{"key", "epoch", "account", "missing-state", "tamper", "expiry-witness", "no-receipt", "cancel"} {
		t.Run(damage, func(t *testing.T) {
			life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			missing := ""
			switch damage {
			case "key":
				key = []byte(strings.Repeat("x", 32))
			case "epoch":
				if err := fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"}); err != nil {
					t.Fatal(err)
				}
			case "account":
				if _, err := accounts.Deactivate(u.ID); err != nil {
					t.Fatal(err)
				}
			case "missing-state":
				missing = filepath.Join(root, "users", u.ID, "state.db")
				if err := os.Remove(missing); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			default:
				f, err := life.load()
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "tamper":
					f.RecoveryReceipt.Body[0] ^= 1
				case "expiry-witness":
					f.RecoveryReceipt.ExpiresAt = f.RecoveryReceipt.ExpiresAt.AddDate(0, 0, 1)
				case "no-receipt":
					f.RecoveryReceipt = nil
				}
				if err = fsutil.PersistJSONFile(life.path, f); err != nil {
					t.Fatal(err)
				}
			}
			all, err := accounts.List()
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(life.path)
			release, err := life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			_, err = life.NativeRecoveryRepairPlanHeld(ctx, root, settings, key, all)
			release()
			if err == nil {
				t.Fatal("unqualified repair plan accepted")
			}
			if damage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			after, _ := os.ReadFile(life.path)
			if !bytes.Equal(before, after) || !errors.Is(RequireNativeRestoreReleased(root), ErrNativeRestoreHold) {
				t.Fatal("refusal modified lifecycle or hold")
			}
			if missing != "" {
				if _, err := os.Stat(missing); !os.IsNotExist(err) {
					t.Fatal("missing storage recreated", err)
				}
			}
		})
	}
}
