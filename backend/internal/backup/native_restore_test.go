//go:build linux

package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
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

func TestNativeRestoreRaisesTokenCutoffPreservesDirectory(t *testing.T) {
	for _, higher := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-cutoff", true: "higher-cutoff"}[higher], func(t *testing.T) {
			s, u := nativeService(t)
			life := sso.NewLifecycleStore(s.dirs.Config)
			if _, err := life.ApplyDirectory("https://legacy.example", syncauth.Event{ID: "legacy", Type: "user.updated", At: time.Now()}, "legacy", 1, "retained", true, func() (bool, error) { return false, nil }); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dirs.Config, "sso-lifecycle.json")
			read := func() map[string]json.RawMessage {
				t.Helper()
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var d map[string]json.RawMessage
				if err := json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				return d
			}
			before := read()
			var directory map[string]sso.DirectoryState
			if err := json.Unmarshal(before["directory"], &directory); err != nil {
				t.Fatal(err)
			}
			key := u.NativeMailboxIssuer + "\x00" + u.SSOSub
			if higher {
				d := directory[key]
				d.RevokedBefore = time.Now().Unix() + 3600
				directory[key] = d
				before["directory"], _ = json.Marshal(directory)
				raw, _ := json.Marshal(before)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			sealKey := pinTestKey(t, s)
			result, err := s.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(result.LocalPath)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "restored")
			if _, _, err := capsule.Open(raw, sealKey, root); err != nil {
				t.Fatal(err)
			}
			path = filepath.Join(root, "config/sso-lifecycle.json")
			before = read()
			for attempt := 0; attempt < 2; attempt++ {
				start := time.Now().Unix()
				if native, err := QuarantineNativeRestore(root); !native || err != nil {
					t.Fatalf("native=%v err=%v", native, err)
				}
				after := read()
				var current map[string]sso.DirectoryState
				if err := json.Unmarshal(after["directory"], &current); err != nil {
					t.Fatal(err)
				}
				d := current[key]
				if d.RevokedBefore < max(directory[key].RevokedBefore, start+31) || d.RevokedBefore > max(directory[key].RevokedBefore, time.Now().Unix()+31) {
					t.Fatal("restore did not fence every previously admissible issuance timestamp")
				}
				floor := d.RevokedBefore
				d.RevokedBefore = directory[key].RevokedBefore
				current[key] = d
				if !reflect.DeepEqual(directory, current) {
					t.Fatal("restore altered directory authority or legacy entries")
				}
				delete(before, "directory")
				delete(after, "directory")
				canonical := func(d map[string]json.RawMessage) map[string]any {
					t.Helper()
					raw, err := json.Marshal(d)
					if err != nil {
						t.Fatal(err)
					}
					var v map[string]any
					if err = json.Unmarshal(raw, &v); err != nil {
						t.Fatal(err)
					}
					return v
				}
				if !reflect.DeepEqual(canonical(before), canonical(after)) {
					t.Fatal("restore altered unrelated lifecycle evidence")
				}
				d.RevokedBefore = floor
				directory[key] = d
				if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(root, "state")), sso.ErrNativeRestoreHold) {
					t.Fatal("cutoff released hold")
				}
			}
		})
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
	for _, damage := range []string{"none", "missing-mailbox", "corrupt-state", "foreign-user", "blocked-carddav-delete"} {
		t.Run(damage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "restored")
			manifest, _, err := capsule.Open(raw, key, dir)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "blocked-carddav-delete":
				path := filepath.Join(dir, "config/users", u.ID, "carddav-auth.json")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "blocker"), []byte("cannot remove nonempty directory"), 0600); err != nil {
					t.Fatal(err)
				}
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
			data, err := os.ReadFile(filepath.Join(dir, "state", sso.NativeRestoreHoldFile))
			if err != nil {
				t.Fatal(err)
			}
			var hold struct {
				Epoch string `json:"epoch"`
			}
			if err := json.Unmarshal(data, &hold); err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(hold.Epoch) {
				t.Fatal("successful or failed restore lacks a valid epoch")
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

func TestNativeRestoreHoldWriteFailurePreservesStaging(t *testing.T) {
	s, u := nativeService(t)
	key := pinTestKey(t, s)
	result, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	if _, _, err := capsule.Open(sealed, key, dir); err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(dir, "state", sso.NativeRestoreHoldFile)
	if err := os.Mkdir(hold, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state/users", u.ID, "mailbox/mailbox.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if native, err := QuarantineNativeRestore(dir); !native || err == nil {
		t.Fatal("failed hold write allowed quarantine", native, err)
	}
	if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
		t.Fatal("unwritable hold stopped blocking native access")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed hold write changed staged mailbox", err)
	}
}

func TestNativeRestoreFatalEntropyHelper(t *testing.T) {
	dir := os.Getenv("KYPOST_TEST_RESTORE_ENTROPY_DIR")
	if dir == "" {
		return
	}
	// Only this subprocess replaces the reader; Go terminates on its error.
	rand.Reader = iotest.ErrReader(errors.New("injected restore entropy failure"))
	_, _ = QuarantineNativeRestore(dir)
	t.Fatal("failed randomness did not terminate the process")
}

func TestNativeRestoreFatalEntropyKeepsHold(t *testing.T) {
	for _, prior := range []string{"", `{"version":1,"epoch":"11111111-1111-4111-8111-111111111111"}`} {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		if err := os.Mkdir(stateDir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(stateDir, sso.NativeRestoreHoldFile)
		if prior != "" {
			if err := os.WriteFile(path, []byte(prior), 0600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeRestoreFatalEntropyHelper$")
		child.Env = append(os.Environ(), "KYPOST_TEST_RESTORE_ENTROPY_DIR="+dir)
		output, err := child.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), "crypto/rand: failed to read random data") {
			t.Fatalf("child did not exercise actual random failure: %v %s", err, output)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("fatal randomness lost the hold", err)
		}
		var hold struct {
			Epoch   string `json:"epoch"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(data, &hold); err != nil || hold.Epoch != "" || hold.Version != 1 {
			t.Fatal("fatal randomness retained a usable or invalid marker", hold, err)
		}
		if !errors.Is(sso.RequireNativeRestoreReleased(stateDir), sso.ErrNativeRestoreHold) {
			t.Fatal("fatal randomness stopped blocking native access")
		}
	}
}

func TestNativeBackupRefusesOrphanMailboxAndForeignReceiving(t *testing.T) {
	for _, damage := range []string{"orphan-mailbox", "foreign-route", "foreign-binding", "missing-binding", "reshaped-archive"} {
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
				if damage == "reshaped-archive" {
					query = `DROP TABLE archived; CREATE TABLE archived(gateway TEXT, id TEXT)`
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
