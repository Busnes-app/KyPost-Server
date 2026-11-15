package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

var (
	migrateOwner  = mailbox.Owner{Issuer: "https://identity.example.test", Subject: "subject-one", Mailbox: "one"}
	migrateExtra  = mailbox.Owner{Issuer: "https://identity.example.test", Subject: "subject-one", Mailbox: "sales"}
	migrateLimits = mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	migrateRaw    = []byte("From: a@outside.test\r\nTo: one@example.test\r\nSubject: kept\r\n\r\nbinary \x00\xff\xfe\r\n")
)

// migrateFixture is a stopped SQLite install under root: the global state, a native
// user with mail (raw BLOBs, a label, an outbox job), a second user, an extra mailbox,
// ingress (its limits) and cloudflare (a status). It returns the state and config dirs.
func migrateFixture(t *testing.T, root string) (stateDir, configDir string, source string) {
	t.Helper()
	ctx := context.Background()
	stateDir, configDir = filepath.Join(root, "state"), filepath.Join(root, "config")
	for _, d := range []string{stateDir, configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	global, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := global.AddDecision(state.Decision{MessageID: "m1", Sender: "a@outside.test", Label: "Work", Status: "labeled", AtUTC: "2026-11-14T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	_ = global.Close()
	if source, err = mailbox.PrepareAccount(stateDir, migrateOwner, "one@example.test", migrateLimits); err != nil {
		t.Fatal(err)
	}
	m, err := mailbox.OpenExisting(filepath.Join(stateDir, "users", "one", "mailbox"), migrateOwner, migrateLimits, source)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: "d1", Sender: "a@outside.test", Recipients: []mailbox.Recipient{{Address: "one@example.test", Generation: 1}}}, bytes.NewReader(migrateRaw))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, "INBOX", id, true, false, []string{"Travel"}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	raw, err := sql.Open("sqlite", filepath.Join(stateDir, "users", "one", "mailbox", "mailbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT OR IGNORE INTO labels VALUES ('Travel'); INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES ('job-1', X'00ff10', 0, 0); INSERT INTO outbox_deliveries(job,sequence,state) VALUES ('job-1', 0, 'queued')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	userState, err := state.OpenNative(filepath.Join(stateDir, "users", "one"), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := userState.UpsertNativeDevice(state.NativeDevice{DeviceID: "phone", UserID: "one", Platform: "android", PushToken: "token"}); err != nil {
		t.Fatal(err)
	}
	_ = userState.Close()
	two, err := state.New(filepath.Join(stateDir, "users", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if err := two.AddDecision(state.Decision{MessageID: "m2", Label: "Bills", Status: "labeled", AtUTC: "2026-11-14T11:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	_ = two.Close()
	if _, err := mailbox.PrepareMailboxContext(ctx, filepath.Join(stateDir, "mailboxes"), migrateExtra, "sales@example.test", migrateLimits); err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(stateDir, "receiving"), ingress.ReceivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	_ = holding.Close()
	cf, err := cfreceiving.Open(filepath.Join(stateDir, "receiving"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cf.SaveStatus(ctx, cfreceiving.Status{State: "running", UpdatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	_ = cf.Close()
	return stateDir, configDir, source
}

// databaseFiles maps every store database under the dirs to its SHA-256.
func databaseFiles(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".db") && !strings.HasSuffix(p, "-wal") {
				return err
			}
			raw, err := os.ReadFile(p)
			sum := sha256.Sum256(raw)
			out[p] = hex.EncodeToString(sum[:])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tableRows reads every row of every table of a SQLite file, sorted, BLOBs as bytes.
func tableRows(t *testing.T, path string) map[string][][]any {
	t.Helper()
	db, err := dbbackend.OpenSQLiteFile(path, "mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var n string
		_ = names.Scan(&n)
		tables = append(tables, n)
	}
	names.Close()
	out := map[string][][]any{}
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2`)
		if err != nil {
			rows, err = db.Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
		}
		if err != nil {
			t.Fatal(table, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], vals)
		}
		rows.Close()
	}
	return out
}

// AC6: the command copies the fixture, prints every table's rows, exits 0, and the
// stores opened on Postgres serve the same rows, byte for byte; the sources are unchanged.
func TestMigrateToPostgres(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	t.Setenv("STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("CONFIG_DIR", filepath.Join(root, "config"))
	var stateDir, configDir, source string
	t.Run("fixture on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		stateDir, configDir, source = migrateFixture(t, root)
	})
	hashes := databaseFiles(t, stateDir, configDir)
	dbbackend.FreshDatabaseForTest(t)
	var out bytes.Buffer
	if err := runMigrateToPostgres([]string{"-from", stateDir, "-config", configDir}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, line := range []string{"state/state.db\tdecisions\t1", "state/users/one/mailbox/mailbox.db\tmessages\t1", "state/users/one/mailbox/mailbox.db\toutbox\t1",
		"state/users/one/state.db\tnative_devices\t1", "state/users/two/state.db\tdecisions\t1", "state/mailboxes/sales/mailbox/mailbox.db\tidentity\t1",
		"state/receiving/ingress.db\tlimits\t1", "state/receiving/cloudflare.db\tstatus\t1", "copied "} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("output lacks %q:\n%s", line, out.String())
		}
	}
	if got := databaseFiles(t, stateDir, configDir); !reflect.DeepEqual(got, hashes) {
		t.Fatal("migration changed a source file")
	}
	// Every table, every row, BLOBs byte-equal: each schema exported back to SQLite.
	for path := range hashes {
		if strings.HasSuffix(path, "-wal") {
			continue
		}
		exported := filepath.Join(t.TempDir(), filepath.Base(path))
		if err := dbbackend.ExportSQLite(context.Background(), path, exported); err != nil {
			t.Fatal(path, err)
		}
		if want, got := tableRows(t, path), tableRows(t, exported); !reflect.DeepEqual(want, got) {
			t.Fatalf("%s differs after migration:\nSQLite   %v\nPostgres %v", path, want, got)
		}
	}
	// The stores the server opens with the variable set.
	m, err := mailbox.OpenExisting(filepath.Join(stateDir, "users", "one", "mailbox"), migrateOwner, migrateLimits, source)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	list, err := m.List(context.Background(), "INBOX", 0, 10)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].Labels, []string{"Travel"}) {
		t.Fatal("migrated inbox", list, err)
	}
	if raw, err := m.Raw(context.Background(), "INBOX", list[0].ID); err != nil || !bytes.Equal(raw, migrateRaw) {
		t.Fatal("migrated message bytes differ", err)
	}
	labels, err := dbbackend.OpenFile(filepath.Join(stateDir, "users", "one", "mailbox", "mailbox.db"), "mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer labels.Close()
	var n int
	if err := labels.QueryRow(`SELECT count(*) FROM labels WHERE name = 'TRAVEL'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("labels do not compare case-insensitively", n, err)
	}
	if pending, err := mailbox.HasPendingOutbound(context.Background(), filepath.Join(stateDir, "users", "one", "mailbox")); err != nil || !pending {
		t.Fatal("migrated outbox job not pending", pending, err)
	}
	global, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer global.Close()
	if d, err := global.DecisionsStrict(10); err != nil || len(d) != 1 || d[0].MessageID != "m1" || d[0].Label != "Work" {
		t.Fatal("migrated decisions", d, err)
	}
	if dbbackend.Name() != "postgres" {
		t.Fatal("backend not reported")
	}
}

// AC7: each refusal exits non-zero with the target unchanged and the sources untouched.
func TestMigrateToPostgresRefusals(t *testing.T) {
	dbbackend.RequirePostgres(t)
	base := t.TempDir()
	t.Run("fixture on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		migrateFixture(t, base)
	})
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, stateDir string) func()
		want  string
	}{
		{"wal beside a source", func(t *testing.T, stateDir string) func() {
			db, err := sql.Open("sqlite", filepath.Join(stateDir, "users", "two", "state.db")+"?_pragma=journal_mode(WAL)")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`INSERT INTO meta VALUES ('unfolded', 'x')`); err != nil {
				t.Fatal(err)
			}
			return func() { _ = db.Close() }
		}, "write-ahead log"},
		{"target holds a row", func(t *testing.T, stateDir string) func() {
			st, err := state.New(filepath.Join(stateDir, "users", "already-on-postgres"))
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			return func() {}
		}, "not empty"},
		{"newer schema", func(t *testing.T, stateDir string) func() {
			db, err := sql.Open("sqlite", filepath.Join(stateDir, "users", "two", "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`ALTER TABLE meta ADD COLUMN future TEXT`); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}, "newer release"},
		{"failure inside the transaction", func(t *testing.T, _ string) func() {
			backup.SetImportHooksForTest(t, func() error { return errors.New("injected failure") }, nil)
			return func() {}
		}, "injected failure"},
		{"recount differs", func(t *testing.T, _ string) func() {
			backup.SetImportHooksForTest(t, nil, func(c backup.Counts) backup.Counts {
				c["state/state.db"]["decisions"]++
				return c
			})
			return func() {}
		}, "were copied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			copyTree(t, base, root)
			stateDir, configDir := filepath.Join(root, "state"), filepath.Join(root, "config")
			t.Setenv("STATE_DIR", stateDir)
			t.Setenv("CONFIG_DIR", configDir)
			dbbackend.FreshDatabaseForTest(t)
			done := c.setup(t, stateDir)
			defer done()
			before, err := dbbackend.RowCounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			hashes := databaseFiles(t, stateDir, configDir)
			var out bytes.Buffer
			err = runMigrateToPostgres([]string{"-from", stateDir, "-config", configDir}, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
			after, err := dbbackend.RowCounts(context.Background())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused migration changed the database:\nbefore %v\nafter  %v (%v)", before, after, err)
			}
			if got := databaseFiles(t, stateDir, configDir); !reflect.DeepEqual(got, hashes) {
				t.Fatal("a refused migration changed a source file")
			}
		})
	}
}

// A file from an older release (a column added since) is brought up to date on a
// private copy; the source keeps its bytes.
func TestMigrateToPostgresUpgradesOlderFileOnACopy(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	t.Setenv("STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("CONFIG_DIR", filepath.Join(root, "config"))
	var stateDir, configDir string
	t.Run("older fixture on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		stateDir, configDir, _ = migrateFixture(t, root)
		db, err := sql.Open("sqlite", filepath.Join(stateDir, "users", "two", "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`ALTER TABLE pull_notifications DROP COLUMN device_ids`); err != nil {
			t.Fatal(err)
		}
	})
	hashes := databaseFiles(t, stateDir, configDir)
	dbbackend.FreshDatabaseForTest(t)
	var out bytes.Buffer
	if err := runMigrateToPostgres([]string{"-from", stateDir, "-config", configDir}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := databaseFiles(t, stateDir, configDir); !reflect.DeepEqual(got, hashes) {
		t.Fatal("the upgrade touched the source")
	}
	two, err := state.New(filepath.Join(stateDir, "users", "two"))
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if err := two.EnqueuePullNotification(state.PullNotification{Title: "t", DeviceIDs: []string{"phone"}}); err != nil {
		t.Fatal("upgraded schema lacks its new column", err)
	}
}

// runRestore with the variable set loads the capsule's databases into Postgres and
// publishes no database file; a second restore into the now non-empty database is refused.
func TestRestoreCLIIntoPostgres(t *testing.T) {
	dbbackend.RequirePostgres(t)
	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	var stateDB []byte
	t.Run("state.db on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		dir := t.TempDir()
		st, err := state.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddDecision(state.Decision{MessageID: "m1", Label: "Work", Status: "labeled", AtUTC: "2026-11-14T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if stateDB, err = os.ReadFile(filepath.Join(dir, "state.db")); err != nil {
			t.Fatal(err)
		}
	})
	raw, _, err := recoveryclient.Seal(recoveryclient.Payload{ServiceName: backup.AppName, AppVersion: "test", Files: []recoveryclient.File{
		{Path: "config/test.json", Data: []byte(`{"synthetic":true}`), Mode: 0600},
		{Path: "state/state.db", Data: stateDB, Mode: 0600},
	}}, recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3})
	if err != nil {
		t.Fatal(err)
	}
	capsulePath := filepath.Join(t.TempDir(), "test.kycap")
	if err := os.WriteFile(capsulePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restore")
	t.Setenv("STATE_DIR", filepath.Join(target, "state"))
	t.Setenv("CONFIG_DIR", filepath.Join(target, "config"))
	dbbackend.FreshDatabaseForTest(t)
	input := shares[0].String() + "\n" + shares[2].String() + "\n"
	var out bytes.Buffer
	if err := runRestore([]string{capsulePath, target}, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(target, "state", "state.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restore into Postgres published a database file", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "config", "test.json")); err != nil || string(got) != `{"synthetic":true}` {
		t.Fatal("other files not published", err)
	}
	st, err := state.New(filepath.Join(target, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if d, err := st.DecisionsStrict(10); err != nil || len(d) != 1 || d[0].MessageID != "m1" {
		t.Fatal("restored decisions", d, err)
	}
	before, err := dbbackend.RowCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runRestore([]string{capsulePath, filepath.Join(t.TempDir(), "again")}, strings.NewReader(input), &out); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("restore into a non-empty database: %v", err)
	}
	if after, err := dbbackend.RowCounts(context.Background()); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("a refused restore changed the database", err)
	}
}
