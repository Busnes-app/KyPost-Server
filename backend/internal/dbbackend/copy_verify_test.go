package dbbackend_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// replicaRows writes one row into every table this job added, BLOBs with NUL and high bytes.
var replicaRows = []string{
	`INSERT INTO sessions VALUES ('h1', 'u1', 'sub', '{"x":1}', 9999999999999)`,
	`INSERT INTO session_revocations VALUES ('u1', 3)`,
	`INSERT INTO security_windows VALUES ('lockout:login:k', 2, 9999999999999)`,
	`INSERT INTO spent_challenges VALUES ('pow:s', 9999999999999)`,
	`INSERT INTO replay_ids VALUES ('kycalendar:e', 'attempt', 9999999999999)`,
	`INSERT INTO auth_challenges VALUES ('mfa:k', 'u1', X'00ff7f10', 9999999999999)`,
	`INSERT INTO import_jobs VALUES ('u1', 'api-1', '{}', 1, 0, 1)`,
	`INSERT INTO replica_instances VALUES ('api-1', 'boot', 'vol', 'api', 1)`,
	`INSERT INTO scheduled_sends (id, user_id, mailbox, send_at, status, summary, created_at, claimant) VALUES ('s1', 'u1', 'u1', 1, 'sending', X'01', 1, 'api-1')`,
	// Identity tables, with ids well above 1.
	`INSERT INTO decisions (id, message_id, at_unix) VALUES (41, 'm', 1)`,
	`INSERT INTO backup_audit (id, at, action, actor, target, outcome, details) VALUES (77, 'now', 'a', 'b', 'c', 'd', '{}')`,
}

// AC23: the copy carries every new table, and its content check fails when one table's rows
// are dropped or one BLOB byte changes on the way, though the counts would still match.
func TestCopyVerifiesEveryTableByContent(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	stateDir, configDir := filepath.Join(root, "state"), filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("fixture on SQLite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		st, err := state.New(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		db, err := dbbackend.OpenSQLiteFile(filepath.Join(stateDir, "state.db"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range replicaRows {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	})
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("CONFIG_DIR", configDir)
	roots := []backup.Root{{Dir: configDir, Prefix: "config"}, {Dir: stateDir, Prefix: "state"}}
	for name, hook := range map[string]func(string, []any) bool{
		"a table left out": func(table string, _ []any) bool { return table == "auth_challenges" },
		"one BLOB byte changed": func(table string, row []any) bool {
			if table == "auth_challenges" {
				for i, v := range row {
					if b, ok := v.([]byte); ok && len(b) > 0 {
						c := append([]byte{}, b...)
						c[0] ^= 1
						row[i] = c
					}
				}
			}
			return false
		},
	} {
		t.Run(name, func(t *testing.T) {
			dbbackend.FreshDatabaseForTest(t)
			dbbackend.SetCopyHookForTest(t, hook)
			if _, err := backup.CheckImport(context.Background(), roots, t.TempDir()); !errors.Is(err, dbbackend.ErrDigestMismatch) {
				t.Fatalf("the copy accepted %s: %v", name, err)
			}
		})
	}
	dbbackend.FreshDatabaseForTest(t)
	counts, err := backup.ImportDatabases(context.Background(), roots, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "session_revocations", "security_windows", "spent_challenges", "replay_ids", "auth_challenges", "import_jobs", "replica_instances", "scheduled_sends"} {
		if counts["state/state.db"][table] != 1 {
			t.Errorf("%s: %d rows copied, want 1", table, counts["state/state.db"][table])
		}
	}
	// Every identity or owned sequence, found from the catalog, hands out a value above the
	// copied maximum: the next insert into each table gets a fresh id.
	ctx := context.Background()
	tx, err := dbbackend.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT n.nspname, c.relname, a.attname, pg_get_serial_sequence(quote_ident(n.nspname) || '.' || quote_ident(c.relname), a.attname)
 FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped AND n.nspname IN (SELECT schema FROM kypost_catalog.stores)
 AND pg_get_serial_sequence(quote_ident(n.nspname) || '.' || quote_ident(c.relname), a.attname) IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	type seq struct{ schema, table, col, seq string }
	var seqs []seq
	for rows.Next() {
		var s seq
		if err := rows.Scan(&s.schema, &s.table, &s.col, &s.seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	if len(seqs) < 2 {
		t.Fatalf("found %d identities; the catalog query is wrong", len(seqs))
	}
	for _, s := range seqs {
		var maxID, next int64
		q := dbbackend.QuoteIdent(s.schema) + "." + dbbackend.QuoteIdent(s.table)
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+dbbackend.QuoteIdent(s.col)+`), 0) FROM `+q).Scan(&maxID); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT nextval($1)`, s.seq).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next <= maxID {
			t.Errorf("%s.%s: the next id %d does not exceed the copied maximum %d", s.table, s.col, next, maxID)
		}
	}
}
