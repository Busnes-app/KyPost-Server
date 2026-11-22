package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/replica"
)

// installSnapshot is every file's bytes (by digest) and every database's schema and rows,
// on SQLite and on PostgreSQL.
func installSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"config", "private", "state"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if strings.HasSuffix(p, ".db-shm") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			out["file "+rel] = hex.EncodeToString(sum[:])
			if strings.HasSuffix(p, ".db") && len(b) > 0 {
				dumpSQLite(t, p, "db "+rel, out)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if dbbackend.Postgres() {
		dumpPostgres(t, out)
	}
	return out
}

func dumpSQLite(t *testing.T, path, prefix string, out map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var tables []string
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatal(err)
		}
		out[prefix+" "+typ+" "+name] = ddl
		if typ == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	rows.Close()
	for _, table := range tables {
		dumpRows(t, db, `SELECT * FROM `+dbbackend.QuoteIdent(table), prefix+" "+table, out)
	}
}

func dumpPostgres(t *testing.T, out map[string]string) {
	t.Helper()
	tx, err := dbbackend.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	dump := func(query, prefix string, args ...any) {
		rows, err := tx.Query(query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		slices.Sort(lines)
		out[prefix] = strings.Join(lines, "\n")
	}
	dump(`SELECT s::text FROM kypost_catalog.stores s`, "pg catalog")
	dump(`SELECT nspname FROM pg_namespace WHERE nspname LIKE 'kypost%'`, "pg schemas")
	var schemas []string
	rows, err := tx.Query(`SELECT schema FROM kypost_catalog.stores`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		schemas = append(schemas, s)
	}
	rows.Close()
	for _, s := range schemas {
		dump(`SELECT table_name||'.'||column_name||' '||data_type||' '||is_nullable||' '||COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema=?`, "pg "+s+" columns", s)
		dump(`SELECT indexdef FROM pg_indexes WHERE schemaname=?`, "pg "+s+" indexes", s)
		dump(`SELECT trigger_name||' '||event_object_table||' '||action_statement FROM information_schema.triggers WHERE trigger_schema=?`, "pg "+s+" triggers", s)
		var tables []string
		trows, err := tx.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema=?`, s)
		if err != nil {
			t.Fatal(err)
		}
		for trows.Next() {
			var n string
			_ = trows.Scan(&n)
			tables = append(tables, n)
		}
		trows.Close()
		for _, table := range tables {
			q := dbbackend.QuoteIdent(s) + "." + dbbackend.QuoteIdent(table)
			dump(`SELECT r::text FROM `+q+` r`, "pg "+s+" "+table)
		}
	}
}

func dumpRows(t *testing.T, db *sql.DB, query, prefix string, out map[string]string) {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%q", vals))
	}
	slices.Sort(lines)
	out[prefix] = strings.Join(lines, "\n")
}

// stopChild sends SIGTERM and waits for a clean exit.
func stopChild(t *testing.T, c *child) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.done:
		c.done <- err
		if err != nil {
			t.Fatalf("%s exited with %v\n%s", c.name, err, c.out.String())
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("%s did not exit", c.name)
	}
}

func diffSnapshots(a, b map[string]string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(a)) {
		if bv, ok := b[k]; !ok {
			out = append(out, "- "+k)
		} else if bv != a[k] {
			out = append(out, "~ "+k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b)) {
		if _, ok := a[k]; !ok {
			out = append(out, "+ "+k)
		}
	}
	return out
}

// TestSingleTenantUpgradeIsByteForByte (tenancy design K1/K3, build plan PR 1): a fixture
// install, restarted under this binary with KYPOST_TENANCY unset and then set to single,
// keeps users.json, every other file and every database's schema and rows exactly as they
// were, sender-blocks.json and sender-evidence.json among them, and no documents table (MH1). On the PostgreSQL job the fixture is a replica install, so the fingerprint record
// is among the rows compared. SQLite's -shm index is rebuilt by every open and is skipped.
// The children cannot reach GitHub, so the release check never records a version.
func TestSingleTenantUpgradeIsByteForByte(t *testing.T) {
	dbURL := ""
	if dbbackend.OnPostgresTest() {
		dbURL = dbbackend.FreshDatabaseForTest(t)
	}
	bin := buildServer(t)
	root := t.TempDir()
	env := append(replicaEnv(root, dbURL), "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost")
	boot := exec.Command(bin, "--mode", "bootstrap-admin")
	boot.Env = env
	if out, err := boot.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap-admin: %v\n%s", err, out)
	}
	probe := &http.Client{Timeout: 3 * time.Second}
	start := func(name string, signin bool, extra ...string) {
		c := startChild(t, bin, name, append(slices.Clone(env), extra...), "--mode", "server")
		waitFor(t, 90*time.Second, name+" ready", func() bool { return status(probe, "GET", c.url("/readyz")) == 200 })
		if signin {
			signIn(t, c)
		}
		stopChild(t, c)
	}
	start("fixture", true)
	// Sender blocks and evidence as single mode writes them (RX PR 3): the files stay.
	receiving := filepath.Join(root, "state", "receiving")
	if err := os.Mkdir(receiving, 0o700); err != nil {
		t.Fatal(err)
	}
	senders, now := ingress.FileSenders(receiving), time.Now()
	if _, err := senders.Put(context.Background(), ingress.SenderBlock{Kind: "domain", Value: "evil.test", Source: "manual", Actor: "a", Reason: "spam"}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := senders.Accepted(context.Background(), ingress.Authentication{Sender: "a@friendly.test", From: "a@friendly.test", SPF: true, DKIM: []string{"friendly.test"}}, now); err != nil {
		t.Fatal(err)
	}
	want := installSnapshot(t, root)
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		if want["file state/receiving/"+name] == "" {
			t.Fatalf("the fixture has no %s", name)
		}
	}
	if want["file config/users.json"] == "" || want["db state/state.db meta"] == "" && want["pg kypost_state meta"] == "" {
		t.Fatalf("the fixture has no users.json or root meta: %v", slices.Sorted(maps.Keys(want)))
	}
	if dbURL != "" && !strings.Contains(want["pg kypost_state meta"], replica.FingerprintSetting) {
		t.Fatalf("the replica fixture has no fingerprint record: %q", want["pg kypost_state meta"])
	}
	for _, v := range []struct{ name, env string }{{"unset", "KYPOST_TENANCY="}, {"single", "KYPOST_TENANCY=single"}} {
		start(v.name, false, v.env)
		// The hosted receiving transport (RX PR 2) refuses and writes nothing: no server
		// key in SECRET_DIR, no tenancy.db, no pin.
		for _, args := range [][]string{{"receiving", "serve", "--tls", "127.0.0.1:2526"}, {"receiving", "gateway", "server-pin"}, {"receiving", "gateway", "add", "mx-a", strings.Repeat("a", 64)}} {
			cmd := exec.Command(bin, args...)
			cmd.Env = append(slices.Clone(env), v.env)
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "multi-tenant install") {
				t.Errorf("KYPOST_TENANCY %s: %v ran: %v %s", v.name, args, err, out)
			}
		}
		got := installSnapshot(t, root)
		if diff := diffSnapshots(want, got); len(diff) > 0 {
			t.Errorf("KYPOST_TENANCY %s changed the install: %v", v.name, diff)
		}
		for k, val := range got {
			// No volume documents either (MH1): single mode keeps every file on its volume.
			if strings.Contains(k, "tenancy") || strings.Contains(val, "tenancy_mode") || strings.Contains(val, "staff_org_id") ||
				strings.HasSuffix(k, " documents") || strings.Contains(k, "documents.") {
				t.Errorf("KYPOST_TENANCY %s left a tenancy trace in %s", v.name, k)
			}
		}
	}
}
