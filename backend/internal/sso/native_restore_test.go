//go:build linux

package sso

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeRestoreSnapshotOwnership(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *LifecycleStore, string, users.User) []users.User
		pass bool
	}{
		{"complete", nil, true},
		{"acknowledged-before-user-publication", func(_ *testing.T, _ *LifecycleStore, _ string, _ users.User) []users.User { return nil }, true},
		{"newer-offboarding-fence", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			nativeDesired(t, s, "one", "one@example.test", 2, false)
			return []users.User{u}
		}, true},
		{"foreign-original-root-is-not-dereferenced", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.StateRoot = "/unrelated-original-host/state" })
			return []users.User{u}
		}, true},
		{"unacknowledged-complete-preparation", func(t *testing.T, s *LifecycleStore, _ string, _ users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.Source, a.Status = "", "pending" })
			return nil
		}, true},
		{"pending-without-storage", func(t *testing.T, s *LifecycleStore, root string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.Source, a.Status = "", "pending" })
			if err := os.RemoveAll(filepath.Join(root, "users", u.ID)); err != nil {
				t.Fatal(err)
			}
			return nil
		}, true},
		{"foreign-user-issuer", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			u.NativeMailboxIssuer = "https://foreign.example"
			return []users.User{u}
		}, false},
		{"foreign-user-subject", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			u.SSOSub = "foreign"
			return []users.User{u}
		}, false},
		{"foreign-user-source", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			u.NativeMailboxSource += "changed"
			return []users.User{u}
		}, false},
		{"unsafe-user-id", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			u.ID = "../elsewhere"
			return []users.User{u}
		}, false},
		{"duplicate-user-id", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User { return []users.User{u, u} }, false},
		{"legacy-then-native-duplicate-subject", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			legacy := users.User{ID: "legacy", SSOSub: u.SSOSub}
			return []users.User{legacy, u}
		}, false},
		{"native-then-legacy-duplicate-subject", func(_ *testing.T, _ *LifecycleStore, _ string, u users.User) []users.User {
			legacy := users.User{ID: "legacy", SSOSub: u.SSOSub}
			return []users.User{u, legacy}
		}, false},
		{"published-without-acknowledgement", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.Source = "" })
			return []users.User{u}
		}, false},
		{"assignment-newer-than-directory", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.Revision++ })
			return []users.User{u}
		}, false},
		{"conflicting-same-revision-digest", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.Digest += "changed" })
			return []users.User{u}
		}, false},
		{"conflicting-same-revision-activity", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			editAssignment(t, s, func(a *NativeAssignment) { a.DesiredActive = false })
			return []users.User{u}
		}, false},
		{"missing-ledger", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			if err := os.Remove(s.nativePath()); err != nil {
				t.Fatal(err)
			}
			return []users.User{u}
		}, false},
		{"missing-lifecycle", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			if err := os.Remove(s.path); err != nil {
				t.Fatal(err)
			}
			return []users.User{u}
		}, false},
		{"missing-domain", func(t *testing.T, s *LifecycleStore, _ string, u users.User) []users.User {
			if err := os.Remove(NewNativeDomainStore(filepath.Dir(s.path)).path); err != nil {
				t.Fatal(err)
			}
			return []users.User{u}
		}, false},
		{"missing-mailbox", removePreparedFile("mailbox/mailbox.db"), false},
		{"missing-state", removePreparedFile("state.db"), false},
		{"missing-manifest", removePreparedFile("native-mailbox.json"), false},
		{"orphan-prepared-storage", func(t *testing.T, s *LifecycleStore, _ string, _ users.User) []users.User {
			if err := os.Remove(s.nativePath()); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(s.path); err != nil {
				t.Fatal(err)
			}
			return nil
		}, false},
		{"orphan-prefix-is-not-exempt", func(t *testing.T, s *LifecycleStore, root string, u users.User) []users.User {
			if err := os.Rename(filepath.Join(root, "users", u.ID), filepath.Join(root, "users", ".native-prepare-published")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(s.nativePath()); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(s.path); err != nil {
				t.Fatal(err)
			}
			return nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, root := t.TempDir(), t.TempDir()
			s := NewLifecycleStore(config)
			accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
			if err != nil {
				t.Fatal(err)
			}
			nativeDesired(t, s, "one", "one@example.test", 1, true)
			u, err := s.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", provenNativeDomain(t, config), accounts, nativeLimits)
			if err != nil {
				t.Fatal(err)
			}
			list := []users.User{u}
			if tc.edit != nil {
				list = tc.edit(t, s, root, u)
			}
			native, err := s.ValidateNativeSnapshot(root, list)
			if !native || (err == nil) != tc.pass {
				t.Fatalf("native=%v err=%v pass=%v", native, err, tc.pass)
			}
			// Validation never repairs an acknowledged missing database.
			if tc.name == "missing-mailbox" {
				if _, err := os.Stat(filepath.Join(root, "users", u.ID, "mailbox/mailbox.db")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("validator recreated missing storage", err)
				}
			}
		})
	}
}

func editAssignment(t *testing.T, s *LifecycleStore, edit func(*NativeAssignment)) {
	t.Helper()
	f, err := s.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	key := directoryKey(nativeIssuer, "one")
	a := f.Accounts[key]
	edit(&a)
	f.Accounts[key] = a
	if err := s.saveNative(f); err != nil {
		t.Fatal(err)
	}
}

func removePreparedFile(rel string) func(*testing.T, *LifecycleStore, string, users.User) []users.User {
	return func(t *testing.T, _ *LifecycleStore, root string, u users.User) []users.User {
		if err := os.Remove(filepath.Join(root, "users", u.ID, rel)); err != nil {
			t.Fatal(err)
		}
		return []users.User{u}
	}
}

func TestNativeRestoreHoldRefusesAllocation(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	s := NewLifecycleStore(config)
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, NativeRestoreHoldFile), []byte("malformed hold still blocks"), 0600); err != nil {
		t.Fatal(err)
	}
	domains := provenNativeDomain(t, config)
	// Configuring the domain creates the empty ledger; allocation must not touch it.
	before, err := os.ReadFile(s.nativePath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AllocateNativeAccount(context.Background(), root, nativeIssuer, "one", domains, accounts, nativeLimits)
	if !errors.Is(err, ErrNativeRestoreHold) {
		t.Fatal("held native allocation proceeded", err)
	}
	if after, err := os.ReadFile(s.nativePath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("held allocation wrote a ledger", err)
	}
}

func TestNativeRestoreTokenCutoffRefusesUnsafeInputsAndWriteFailure(t *testing.T) {
	for _, damage := range []string{"missing-epoch", "bad-epoch", "public-epoch", "missing-directory", "foreign-owner", "blocked-write"} {
		t.Run(damage, func(t *testing.T) {
			life, root, _, _, accounts, u := publishedRecoveryFixture(t, true)
			all, err := accounts.List()
			if err != nil {
				t.Fatal(err)
			}
			hold := filepath.Join(root, NativeRestoreHoldFile)
			switch damage {
			case "missing-epoch":
				if err = os.Remove(hold); err != nil {
					t.Fatal(err)
				}
			case "bad-epoch":
				if err = os.WriteFile(hold, []byte(`{"version":1,"epoch":"invalid"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "public-epoch":
				if err = os.Chmod(hold, 0644); err != nil {
					t.Fatal(err)
				}
			case "missing-directory":
				f, e := life.load()
				if e != nil {
					t.Fatal(e)
				}
				delete(f.Directory, directoryKey(u.NativeMailboxIssuer, u.SSOSub))
				if err = fsutil.PersistJSONFile(life.path, f); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				for i := range all {
					if all[i].ID == u.ID {
						all[i].NativeMailboxIssuer = "https://foreign.example"
					}
				}
			case "blocked-write":
				dir := filepath.Dir(life.path)
				if err = os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			}
			before, err := os.ReadFile(life.path)
			if err != nil {
				t.Fatal(err)
			}
			if err = life.FenceRestoredNativeTokens(root, all); err == nil {
				t.Fatal("unsafe cutoff mutation accepted")
			}
			after, err := os.ReadFile(life.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed cutoff mutation changed lifecycle")
			}
		})
	}
}

func TestNativeRestoreTokenCutoffSurvivesDirectoryUpdate(t *testing.T) {
	life, root, _, _, accounts, _ := publishedRecoveryFixture(t, true)
	all, err := accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if err = life.FenceRestoredNativeTokens(root, all); err != nil {
		t.Fatal(err)
	}
	before, known, err := life.Directory(nativeIssuer, "one")
	if err != nil || !known {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 2, false)
	after, known, err := life.Directory(nativeIssuer, "one")
	if err != nil || !known {
		t.Fatal(err)
	}
	if after.Revision != 2 || after.Active || after.RevokedBefore != before.RevokedBefore {
		t.Fatal("newer authority lowered restored cutoff")
	}
}
