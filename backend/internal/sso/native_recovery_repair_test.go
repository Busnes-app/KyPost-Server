//go:build linux

package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func applyPublishedRecoveryBatch(t *testing.T, life *LifecycleStore, root string, settings SSOSettings, key []byte, accounts *users.Store, stopAfterIntent bool) error {
	t.Helper()
	ctx := context.Background()
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var plan []users.NativeAccountRepair
	err = accounts.WithCurrentUsers(ctx, func(all []users.User) error {
		var err error
		plan, err = life.NativeRecoveryRepairPlanHeld(ctx, root, settings, key, all)
		return err
	})
	if err != nil {
		return err
	}
	return accounts.RepairNativeAccounts(ctx, settings.IssuerURL, plan, func(current, repaired []users.User) (func(), error) {
		if err := life.RecordNativeRecoveryRepairIntentHeld(ctx, root, settings, key, current, repaired); err != nil {
			return nil, err
		}
		if stopAfterIntent {
			return nil, errors.New("simulated stop after durable intent before account write")
		}
		return nil, nil
	})
}

func completePublishedRecoveryBatch(t *testing.T, life *LifecycleStore, root string, settings SSOSettings, key []byte, accounts *users.Store) error {
	t.Helper()
	release, err := life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	return accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
		// Component check only: no API or credential cleanup is claimed here.
		return life.CompleteNativeRecoveryRepairHeld(context.Background(), root, settings, key, all)
	})
}

func TestNativeRecoveryRepairIntentPrecedesAccountsAndRetainsHold(t *testing.T) {
	for _, stopAfterIntent := range []bool{true, false} {
		name := "accounts-written"
		if stopAfterIntent {
			name = "interrupted-before-accounts"
		}
		t.Run(name, func(t *testing.T) {
			life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
			before, _ := accounts.List()
			oldDirectory, _, _ := life.Directory(settings.IssuerURL, u.SSOSub)
			err := applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, stopAfterIntent)
			if (err != nil) != stopAfterIntent {
				t.Fatal("unexpected repair result", err)
			}
			f, err := life.load()
			k := directoryKey(settings.IssuerURL, u.SSOSub)
			if err != nil || f.RecoveryRepair == nil || f.RecoveryRepair.CompletedAt != nil || f.RecoveryFloors[k] != 1 || !f.RecoveryRepairBarriers[k].matches(u, 1) || !reflect.DeepEqual(oldDirectory, f.Directory[k]) {
				t.Fatal("intent/floor missing or directory mutated", err)
			}
			after, _ := accounts.List()
			if stopAfterIntent && !reflect.DeepEqual(before, after) {
				t.Fatal("accounts changed despite interruption")
			}
			if err = applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, false); err == nil {
				t.Fatal("existing repair journal silently reapplied")
			}
			for _, eventID := range []string{"one-1", "another-queued-event"} {
				_, err := life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: eventID, Type: "user.updated", At: time.Now()}, u.SSOSub, 1, oldDirectory.Digest, true, func() (bool, error) { t.Fatal("queued old authority applied"); return false, nil })
				if !errors.Is(err, ErrDirectoryConflict) {
					t.Fatal("old revision bypassed barrier/replay fence", err)
				}
			}
			err = completePublishedRecoveryBatch(t, life, root, settings, key, accounts)
			if (err != nil) != stopAfterIntent {
				t.Fatal("completion did not require expected after state", err)
			}
			if !stopAfterIntent {
				f, _ = life.load()
				if f.RecoveryRepair.CompletedAt == nil || completePublishedRecoveryBatch(t, life, root, settings, key, accounts) == nil {
					t.Fatal("completion absent or repeated completion admitted")
				}
			}
			if !errors.Is(RequireNativeRestoreReleased(root), ErrNativeRestoreHold) {
				t.Fatal("repair released hold")
			}
		})
	}
}

func TestNativeRecoveryRepairProvenanceSurvivesFreshRestoreButRejectsMismatch(t *testing.T) {
	for _, damage := range []string{"none", "source", "mailbox", "subject", "issuer", "revision", "version", "missing", "nonce", "epoch"} {
		t.Run(damage, func(t *testing.T) {
			life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
			if err := applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, true); err == nil {
				t.Fatal("expected interrupted repair")
			}
			f, err := life.load()
			if err != nil {
				t.Fatal(err)
			}
			k := directoryKey(settings.IssuerURL, u.SSOSub)
			barrier := f.RecoveryRepairBarriers[k]
			switch damage {
			case "source":
				barrier.Source = "native:foreign"
			case "mailbox":
				barrier.Mailbox = "foreign"
			case "subject":
				barrier.Subject = "foreign"
			case "issuer":
				barrier.Issuer = "https://foreign.test"
			case "revision":
				barrier.Revision++
			case "version":
				barrier.Version++
			case "nonce":
				barrier.Nonce = "malformed"
			case "epoch":
				barrier.Epoch = "malformed"
			}
			f.RecoveryRepairBarriers[k] = barrier
			if damage == "missing" {
				delete(f.RecoveryRepairBarriers, k)
			}
			if err = fsutil.PersistJSONFile(life.path, f); err != nil {
				t.Fatal(err)
			}
			// A later offline restore changes the epoch, not immutable ownership.
			if err = fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"}); err != nil {
				t.Fatal(err)
			}
			fingerprint := sha256.Sum256(key)
			before, _ := os.ReadFile(life.path)
			release, err := life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			err = accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
				_, err := life.BeginNativeRecoveryHeld(context.Background(), root, settings, key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
				return err
			})
			release()
			if (err == nil) != (damage == "none") {
				t.Fatal("incorrect provenance acceptance", err)
			}
			if damage != "none" {
				after, _ := os.ReadFile(life.path)
				if !bytes.Equal(before, after) {
					t.Fatal("mismatched provenance changed lifecycle")
				}
			} else {
				f, _ = life.load()
				if f.RecoveryRepair != nil || f.RecoveryReceipt != nil || f.RecoveryChallenge.Epoch == barrier.Epoch || f.RecoveryFloors[k] != 1 || !reflect.DeepEqual(f.RecoveryRepairBarriers[k], barrier) {
					t.Fatal("new challenge lost ordering or retained qualification")
				}
			}
		})
	}
}

func TestNativeRecoveryRepairCompletionRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"local-account", "new-challenge", "new-event", "key", "epoch", "signed-expiry", "receipt-tamper"} {
		t.Run(change, func(t *testing.T) {
			life, root, settings, key, accounts, u := publishedRecoveryFixture(t, false)
			if err := applyPublishedRecoveryBatch(t, life, root, settings, key, accounts, false); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "local-account":
				if _, err := accounts.Reactivate(u.ID); err != nil {
					t.Fatal(err)
				}
			case "new-challenge":
				fingerprint := sha256.Sum256(key)
				release, err := life.LockDirectory()
				if err != nil {
					t.Fatal(err)
				}
				err = accounts.WithCurrentUsers(context.Background(), func(all []users.User) error {
					_, err := life.BeginNativeRecoveryHeld(context.Background(), root, settings, key, all, "paired-system", hex.EncodeToString(fingerprint[:]))
					return err
				})
				release()
				if err != nil {
					t.Fatal(err)
				}
			case "new-event":
				_, err := life.ApplyDirectory(settings.IssuerURL, syncauth.Event{ID: "newer", Type: "user.updated", At: time.Now()}, u.SSOSub, 2, "newer-digest", false, func() (bool, error) { return true, nil })
				if err != nil {
					t.Fatal(err)
				}
			case "key":
				key = []byte(strings.Repeat("x", 32))
			case "epoch":
				if err := fsutil.PersistJSONFile(filepath.Join(root, NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abd"}); err != nil {
					t.Fatal(err)
				}
			default:
				f, err := life.load()
				if err != nil {
					t.Fatal(err)
				}
				if change == "receipt-tamper" {
					f.RecoveryReceipt.Body[0] ^= 1
				} else {
					var evidence nativeRecoveryEvidence
					if err = json.Unmarshal(f.RecoveryReceipt.Body, &evidence); err != nil {
						t.Fatal(err)
					}
					evidence.IssuedAt = time.Now().Add(-2 * time.Minute)
					evidence.ExpiresAt = time.Now().Add(-time.Second)
					f.RecoveryReceipt.Body, err = json.Marshal(evidence)
					if err != nil {
						t.Fatal(err)
					}
					f.RecoveryReceipt.Headers, err = syncauth.Sign(key, evidence.IssuedAt, "recovery.evidence", evidence.Nonce, f.RecoveryReceipt.Body)
					if err != nil {
						t.Fatal(err)
					}
					f.RecoveryReceipt.Challenge.CreatedAt = evidence.IssuedAt.Add(-time.Second)
					f.RecoveryReceipt.ExpiresAt = evidence.ExpiresAt
					f.RecoveryRepair.ExpiresAt = minRecoveryExpiry(f.RecoveryReceipt)
					f.RecoveryRepair.ReceiptDigest, err = nativeRecoveryReceiptDigest(f.RecoveryReceipt)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err = fsutil.PersistJSONFile(life.path, f); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(life.path)
			if err := completePublishedRecoveryBatch(t, life, root, settings, key, accounts); err == nil {
				t.Fatal("changed authority completed repair")
			}
			after, _ := os.ReadFile(life.path)
			f, _ := life.load()
			k := directoryKey(settings.IssuerURL, u.SSOSub)
			if !bytes.Equal(before, after) || f.RecoveryFloors[k] != 1 || !errors.Is(RequireNativeRestoreReleased(root), ErrNativeRestoreHold) {
				t.Fatal("refusal weakened barriers or hold")
			}
			if change == "new-event" && (f.RecoveryRepair != nil || f.RecoveryReceipt != nil) {
				t.Fatal("new event retained qualification")
			}
		})
	}
}

func TestNativeRecoveryRepairIntentRejectsUnplannedAccountChanges(t *testing.T) {
	for _, field := range []string{"password", "wrapped-key", "role", "epoch", "legacy-role"} {
		t.Run(field, func(t *testing.T) {
			life, root, settings, key, accounts, native := publishedRecoveryFixture(t, false)
			ctx := context.Background()
			release, err := life.LockDirectory()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var plan []users.NativeAccountRepair
			err = accounts.WithCurrentUsers(ctx, func(all []users.User) error {
				var err error
				plan, err = life.NativeRecoveryRepairPlanHeld(ctx, root, settings, key, all)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := accounts.List()
			lifecycle, _ := os.ReadFile(life.path)
			err = accounts.RepairNativeAccounts(ctx, settings.IssuerURL, plan, func(current, repaired []users.User) (func(), error) {
				for i := range repaired {
					if repaired[i].ID == native.ID {
						switch field {
						case "password":
							repaired[i].PasswordHash = "unplanned"
						case "wrapped-key":
							repaired[i].PGPPrivateKeyWrapped = "unplanned"
						case "role":
							repaired[i].Role = users.RoleAdmin
						case "epoch":
							repaired[i].NativeSendEpoch++
						}
					} else if field == "legacy-role" {
						repaired[i].Role = users.RoleUser
					}
				}
				return nil, life.RecordNativeRecoveryRepairIntentHeld(ctx, root, settings, key, current, repaired)
			})
			if err == nil {
				t.Fatal("unplanned account changes qualified")
			}
			after, _ := accounts.List()
			currentLifecycle, _ := os.ReadFile(life.path)
			if !reflect.DeepEqual(before, after) || !bytes.Equal(lifecycle, currentLifecycle) {
				t.Fatal("invalid batch changed durable data")
			}
		})
	}
}
