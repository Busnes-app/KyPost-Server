//go:build linux

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The same native install with a paired device and a message, sealed on the backend the
// test runs on.
type sealedFixture struct {
	sealed  []byte
	key     recoverykey.PrivateKey
	members []string
	user    users.User
	raw     []byte
}

func sealFixture(t *testing.T) sealedFixture {
	t.Helper()
	ctx := context.Background()
	s, u := nativeService(t)
	assignment, _, err := sso.NewLifecycleStore(s.dirs.Config).NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	account := filepath.Join(s.dirs.State, "users", u.ID)
	st, err := state.OpenNative(account, u.NativeMailboxSource)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedRestoreDevice(t, st, u.ID)
	mail, err := mailbox.OpenExisting(filepath.Join(account, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer mail.Close()
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: kept\r\n\r\nbinary \x00\xff\xfe body\r\n")
	receipt := mailbox.Receipt{Gateway: "maddy", Delivery: "d1", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: "one@example.test", Generation: assignment.Revision}}}
	if _, err := mail.Import(ctx, receipt, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	key := pinTestKey(t, s)
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, _, err := capsule.Open(sealed, key, filepath.Join(t.TempDir(), "members"))
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, f := range opened.Files {
		members = append(members, f.Path)
	}
	slices.Sort(members)
	return sealedFixture{sealed, key, members, u, raw}
}

// restoreStaging opens the capsule into staging and runs the restore CLI's holds and
// revocations, as runRestore does before publication.
func restoreStaging(t *testing.T, f sealedFixture) (string, capsule.Manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	m, _, err := capsule.Open(f.sealed, f.key, dir)
	if err != nil {
		t.Fatal(err)
	}
	if native, err := QuarantineNativeRestore(dir); !native || err != nil {
		t.Fatal("quarantine", native, err)
	}
	if err := state.FenceRestoredScheduledSends(filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	if err := state.FenceRestoredAgentTokens(filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// checkRestored is the restore qualification a restored install must show: the hold in
// place, the device credential revoked, the mail kept byte for byte.
func checkRestored(t *testing.T, f sealedFixture, data string) {
	t.Helper()
	if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(data, "state")), sso.ErrNativeRestoreHold) {
		t.Fatal("restore did not hold native mail")
	}
	assignment, _, err := sso.NewLifecycleStore(filepath.Join(data, "config")).NativeAssignment(f.user.NativeMailboxIssuer, f.user.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	account := filepath.Join(data, "state", "users", f.user.ID)
	st, err := state.OpenNative(account, f.user.NativeMailboxSource)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.GetNativeDevice("old-device"); ok {
		t.Fatal("restored device credential still resolves")
	}
	if c, err := st.Checkpoint(); err != nil || c != "retained-checkpoint" {
		t.Fatal("restored checkpoint", c, err)
	}
	mail, err := mailbox.OpenExisting(filepath.Join(account, "mailbox"), assignment.Owner, assignment.Limits, assignment.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer mail.Close()
	list, err := mail.List(context.Background(), "INBOX", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatal("restored inbox", list, err)
	}
	got, err := mail.Raw(context.Background(), "INBOX", list[0].ID)
	if err != nil || !bytes.Equal(got, f.raw) {
		t.Fatalf("restored message bytes differ: %v", err)
	}
}

// AC9: a capsule sealed on Postgres has a SQLite install's members, passes the drill and
// restores onto SQLite; a SQLite capsule restores into Postgres; a non-empty Postgres
// database is refused unchanged.
func TestPostgresCapsuleRoundTrips(t *testing.T) {
	dbbackend.RequirePostgres(t)
	var onSQLite, onPostgres sealedFixture
	t.Run("seal on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		onSQLite = sealFixture(t)
	})
	t.Run("seal on Postgres", func(t *testing.T) {
		onPostgres = sealFixture(t)
	})
	if len(onSQLite.members) == 0 || !slices.Equal(onSQLite.members, onPostgres.members) {
		t.Fatalf("capsule members differ:\nSQLite   %v\nPostgres %v", onSQLite.members, onPostgres.members)
	}
	// As a server runs: the fixture's dirs are STATE_DIR and CONFIG_DIR, so Held routes
	// every live path to Postgres and only state/backup-scratch stays SQLite.
	t.Run("seal on Postgres with live names", func(t *testing.T) {
		dbbackend.FreshDatabaseForTest(t)
		// fixtureDirs takes the test's next temp dirs for config, then state.
		root := filepath.Dir(t.TempDir())
		t.Setenv("CONFIG_DIR", filepath.Join(root, "002"))
		t.Setenv("STATE_DIR", filepath.Join(root, "003"))
		live := sealFixture(t)
		if !slices.Equal(onSQLite.members, live.members) {
			t.Fatalf("capsule members differ:\nSQLite %v\nlive   %v", onSQLite.members, live.members)
		}
		stores, err := dbbackend.Stores(context.Background())
		if err != nil || !slices.ContainsFunc(stores, func(s dbbackend.Store) bool { return s.Path == "state/state.db" }) || !slices.ContainsFunc(stores, func(s dbbackend.Store) bool { return s.Path == "config/native-authority.db" }) {
			t.Fatalf("the fixture did not run under live names: %+v %v", stores, err)
		}
	})
	t.Run("Postgres capsule drills and restores onto SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		dir, m := restoreStaging(t, onPostgres)
		for _, c := range drillChecks(dir, m) {
			if !c.Passed {
				t.Fatalf("drill check %s failed: %s", c.Name, c.Message)
			}
		}
		checkRestored(t, onPostgres, dir)
		if p3, err := releaseP3(t, dir); err != nil || !p3.OK {
			t.Fatalf("restore qualification on SQLite: %+v %v", p3, err)
		}
	})
	t.Run("SQLite capsule restores into Postgres", func(t *testing.T) {
		dbbackend.FreshDatabaseForTest(t)
		dir, _ := restoreStaging(t, onSQLite)
		t.Setenv("STATE_DIR", filepath.Join(dir, "state"))
		t.Setenv("CONFIG_DIR", filepath.Join(dir, "config"))
		roots := []Root{{Dir: filepath.Join(dir, "config"), Prefix: "config"}, {Dir: filepath.Join(dir, "state"), Prefix: "state"}}
		copied, err := ImportDatabases(context.Background(), roots, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(copied) == 0 {
			t.Fatal("nothing imported")
		}
		if err := RemoveDatabaseFiles(roots); err != nil {
			t.Fatal(err)
		}
		checkRestored(t, onSQLite, dir)
		// The release gate reads the Postgres mailboxes, and finds their sendable work.
		if p3, err := releaseP3(t, dir); err != nil || !p3.OK {
			t.Fatalf("restore qualification on Postgres: %+v %v", p3, err)
		}
		mail, err := dbbackend.OpenFile(filepath.Join(dir, "state", "users", onSQLite.user.ID, "mailbox", "mailbox.db"), "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = mail.Exec(`INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES ('job-1', '\x00ff'::bytea, 0, 0); INSERT INTO outbox_deliveries(job,sequence,state) VALUES ('job-1', 0, 'queued')`)
		_ = mail.Close()
		if err != nil {
			t.Fatal(err)
		}
		p3, err := releaseP3(t, dir)
		if p3.OK || err == nil || !strings.Contains(strings.Join(p3.Reasons, " "), "queued or retryable outbound deliveries") {
			t.Fatalf("a queued Postgres outbox passed the release gate: %+v %v", p3, err)
		}
		if _, err := mail.Exec(`SELECT 1`); err == nil {
			t.Fatal("closed pool still usable")
		}
		cleanup, err := dbbackend.OpenFile(filepath.Join(dir, "state", "users", onSQLite.user.ID, "mailbox", "mailbox.db"), "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = cleanup.Exec(`DELETE FROM outbox_deliveries; DELETE FROM outbox`)
		_ = cleanup.Close()
		if err != nil {
			t.Fatal(err)
		}

		before, err := dbbackend.RowCounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		again, _ := restoreStaging(t, onSQLite)
		_, err = ImportDatabases(context.Background(), []Root{{Dir: filepath.Join(again, "config"), Prefix: "config"}, {Dir: filepath.Join(again, "state"), Prefix: "state"}}, t.TempDir())
		if !errors.Is(err, dbbackend.ErrNotEmpty) {
			t.Fatalf("restore into a non-empty database: %v", err)
		}
		after, err := dbbackend.RowCounts(context.Background())
		if err != nil || !sameCounts(before, after) {
			t.Fatalf("a refused restore changed the database: %v", err)
		}
	})
}

// releaseP3 is the release gate's restore-qualification precondition on a restored
// tree, and CheckNativeRestoreQualification's own answer.
func releaseP3(t *testing.T, data string) (sso.NativeRestoreReleaseCheck, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(data, "config", "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Users []users.User `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	life := sso.NewLifecycleStore(filepath.Join(data, "config"))
	_, qualErr := life.CheckNativeRestoreQualification(filepath.Join(data, "state"))
	st := life.NativeRestoreReleaseStatus(filepath.Join(data, "state"), t.TempDir(), sso.NewStore(filepath.Join(data, "config")).Load(), make([]byte, 32), doc.Users, time.Now().UTC())
	for _, c := range st.Preconditions {
		if c.ID == "P3" {
			return c, qualErr
		}
	}
	t.Fatalf("release status has no P3: %+v", st)
	return sso.NativeRestoreReleaseCheck{}, nil
}

func sameCounts(a, b map[string]map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for rel, tables := range a {
		for table, n := range tables {
			if b[rel][table] != n {
				return false
			}
		}
	}
	return true
}
