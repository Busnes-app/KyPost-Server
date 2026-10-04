//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func nativeService(t *testing.T) (*Service, users.User) {
	t.Helper()
	s := validService(t)
	ctx := context.Background()
	issuer := "https://identity.example.test"
	domains := sso.NewNativeDomainStore(s.dirs.Config)
	proof, err := domains.Configure(ctx, "example.test", issuer)
	if err != nil {
		t.Fatal(err)
	}
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return []string{proof.RecordValue()}, nil })
	if _, err := domains.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	life := sso.NewLifecycleStore(s.dirs.Config)
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":true,"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var desired sso.DirectoryUser
	if err := json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	event := syncauth.Event{ID: "one-1", Type: "user.updated", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(issuer, event, desired, sso.EventDigest(event.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	a, err := life.ReconcileNativeMailbox(s.dirs.State, issuer, "one", "native-one", "example.test", limits)
	if err != nil {
		t.Fatal(err)
	}
	u := users.User{ID: a.Owner.Mailbox, Username: "one", Role: users.RoleAdmin, Active: true, SSOSub: "one", NativeMailboxIssuer: issuer, NativeMailboxSource: a.Source}
	writeNativeUsers(t, s, u)
	return s, u
}

func writeNativeUsers(t *testing.T, s *Service, u users.User) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"users": []users.User{u}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dirs.Config, "users.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeBackupRestoreOwnershipAndHold(t *testing.T) {
	s, u := nativeService(t)
	key := pinTestKey(t, s)
	result, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"none", "missing-mailbox", "corrupt-state", "foreign-user"} {
		t.Run(damage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "restored")
			manifest, _, err := capsule.Open(raw, key, dir)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing-mailbox":
				if err := os.Remove(filepath.Join(dir, "state/users", u.ID, "mailbox/mailbox.db")); err != nil {
					t.Fatal(err)
				}
			case "corrupt-state":
				if err := os.WriteFile(filepath.Join(dir, "state/users", u.ID, "state.db"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-user":
				foreign := u
				foreign.NativeMailboxIssuer = "https://foreign.example"
				data, err := json.Marshal(map[string]any{"users": []users.User{foreign}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config/users.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			native, err := QuarantineNativeRestore(dir)
			if !native || (err == nil) != (damage == "none") {
				t.Fatalf("native=%v damage=%s err=%v", native, damage, err)
			}
			if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
				t.Fatal("restore did not persist a hold")
			}
			if _, err := os.Stat(filepath.Join(dir, "config/native-provisioning.json")); err != nil {
				t.Fatal("restore erased ownership", err)
			}
			switch damage {
			case "none":
				for _, check := range drillChecks(dir, manifest) {
					if !check.Passed {
						t.Errorf("check failed: %s", check.Name)
					}
				}
			case "missing-mailbox":
				if _, err := os.Stat(filepath.Join(dir, "state/users", u.ID, "mailbox/mailbox.db")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("restore recreated lost mailbox", err)
				}
			}
		})
	}
	// Verify bytes assembled by collection too, before sealing/releasing a capsule.
	u.NativeMailboxSource += "foreign"
	writeNativeUsers(t, s, u)
	if _, err := s.Collect(); err == nil {
		t.Fatal("collection sealed mismatched ownership")
	}
}

func TestDatabaseOnlyNativeRestoreStaysHeld(t *testing.T) {
	for _, name := range []string{"mailbox.db", "ingress.db"} {
		t.Run(name, func(t *testing.T) {
			s := validService(t)
			// A valid SQLite file without its native ownership metadata is unsafe
			// even when the integrity check itself passes.
			raw, err := os.ReadFile(filepath.Join(s.dirs.State, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "restore")
			if err := os.MkdirAll(filepath.Join(dir, "state/receiving"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config/users.json"), []byte(`{"users":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "state/receiving", name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			native, err := QuarantineNativeRestore(dir)
			if !native || err == nil {
				t.Fatalf("database-only restore escaped hold: native=%v err=%v", native, err)
			}
			if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
				t.Fatal("database-only restore hold absent")
			}
			if err := os.WriteFile(filepath.Join(s.dirs.State, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Collect(); err == nil {
				t.Fatal("collector sealed unowned native database")
			}
		})
	}
}

func TestNativeBackupRefusesOrphanMailboxAndForeignReceiving(t *testing.T) {
	for _, damage := range []string{"orphan-mailbox", "foreign-route", "foreign-binding", "missing-binding"} {
		t.Run(damage, func(t *testing.T) {
			s, u := nativeService(t)
			if damage == "orphan-mailbox" {
				path := filepath.Join(s.dirs.State, "users/unregistered/mailbox")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(s.dirs.State, "users", u.ID, "mailbox/mailbox.db"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "mailbox.db"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				dir := filepath.Join(s.dirs.State, "receiving")
				g, err := ingress.Open(dir, ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
				if err != nil {
					t.Fatal(err)
				}
				if err := g.SetRoute(context.Background(), ingress.Route{Address: "one@example.test", Issuer: u.NativeMailboxIssuer, Subject: u.SSOSub, Mailbox: u.ID, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				if err := g.Bind(context.Background(), "gateway", "one", "sender@example.test", "one@example.test"); err != nil {
					t.Fatal(err)
				}
				if err := g.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "ingress.db"))
				if err != nil {
					t.Fatal(err)
				}
				query := `UPDATE routes SET subject='foreign'`
				if damage == "foreign-binding" {
					query = `UPDATE bindings SET mailbox='foreign'`
				}
				if damage == "missing-binding" {
					query = `UPDATE deliveries SET state='pending'; DELETE FROM bindings`
				}
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Collect(); err == nil {
				t.Fatalf("collector accepted %s", damage)
			}
		})
	}
}
