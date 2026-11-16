package users

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// WithCurrentUsers reuses its last decode only for identical bytes: another
// process's write is seen, and a callback cannot edit the reused snapshot.
func TestCurrentUsersSnapshotIsFreshAndPrivate(t *testing.T) {
	ctx := context.Background()
	s, path := newCacheTestStore(t)
	other, err := OpenExisting(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		t.Helper()
		if err := s.WithCurrentUsers(ctx, func(all []User) error {
			n = len(all)
			all[0].Username = "edited-by-callback"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	if _, err := other.Create(ctx, "second", "correct horse battery staple", RoleUser); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("snapshot has %d users after another store's write, want %d", got, before+1)
	}
	if err := s.WithCurrentUsers(ctx, func(all []User) error {
		if all[0].Username == "edited-by-callback" {
			t.Fatal("a callback's edit reached the next snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentUsersSnapshotNestedFieldsArePrivate(t *testing.T) {
	want := User{
		ID: "test-user", Username: "original",
		RecoveryCodesHash:   []string{"original-code"},
		PGPWrappedEnvelopes: []WrappedEnvelope{{Slot: "recovery", Envelope: "original-envelope"}},
		PGPKeyring:          &PGPKeyringState{Version: 1, MaterialGeneration: 1, KeyFingerprints: []string{"key"}, PrimaryFingerprints: []string{"primary"}},
		SendQuota:           &config.SendQuota{MessagesPerHour: 10, RecipientsPerDay: 20},
	}
	for name, mutate := range map[string]func(*User){
		"recovery codes":       func(u *User) { u.RecoveryCodesHash[0] = "changed" },
		"wrapped envelopes":    func(u *User) { u.PGPWrappedEnvelopes[0].Envelope = "changed" },
		"keyring pointer":      func(u *User) { u.PGPKeyring.MaterialGeneration++ },
		"key fingerprints":     func(u *User) { u.PGPKeyring.KeyFingerprints[0] = "changed" },
		"primary fingerprints": func(u *User) { u.PGPKeyring.PrimaryFingerprints[0] = "changed" },
		"quota pointer":        func(u *User) { u.SendQuota.MessagesPerHour++ },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.json")
			raw, err := json.Marshal(usersFile{Version: 1, Users: []User{want}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			s := newStore(path)
			if err := s.WithCurrentUsers(context.Background(), func(all []User) error {
				mutate(&all[0])
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.WithCurrentUsers(context.Background(), func(all []User) error {
				if !reflect.DeepEqual(all[0], want) {
					t.Error("callback mutation reached the next snapshot")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
