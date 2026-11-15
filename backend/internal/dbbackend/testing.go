package dbbackend

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

// Tests run their stores on Postgres when KYPOST_TEST_DATABASE_URL is set: a store at a
// path outside the production roots gets a schema named after the absolute path
// (kt_<hash>), so two opens of one path share it, as two handles on one file share the
// file, and different tests are isolated. A later run drops the schemas of tests whose
// directory is gone.

func testHook() bool { return testing.Testing() }

// productionNames is set by FreshDatabaseForTest: the test's stores take their
// production names under STATE_DIR and CONFIG_DIR, as a server's would.
var productionNames atomic.Bool

func testNames() bool { return testHook() && !productionNames.Load() }

// OnPostgresTest reports whether this test binary runs its stores on Postgres.
func OnPostgresTest() bool { return testHook() && Postgres() }

// SkipOnPostgres skips a test about SQLite itself (its files, pragmas or catalog).
func SkipOnPostgres(t testing.TB, reason string) {
	t.Helper()
	if OnPostgresTest() {
		t.Skip("SQLite only: " + reason)
	}
}

// RequirePostgres skips a Postgres-only test when KYPOST_TEST_DATABASE_URL is unset.
func RequirePostgres(t testing.TB) {
	t.Helper()
	if !OnPostgresTest() {
		t.Skip("Postgres only: set " + EnvTestURL)
	}
}

// FreshDatabaseForTest creates an empty database on the test server (UTF8, C locale) and
// points this process at it, with production schema names, until the test ends. Tests
// using it must not run in parallel with other store tests of the same binary.
func FreshDatabaseForTest(t testing.TB) string {
	t.Helper()
	RequirePostgres(t)
	base := current().url
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "kt_db_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(`CREATE DATABASE ` + name + ` ENCODING 'UTF8' LOCALE 'C' TEMPLATE template0`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgdialect.Config(base)
	if err != nil {
		t.Fatal(err)
	}
	fresh := strings.Replace(base, "/"+cfg.Database, "/"+name, 1)
	SetURLForTest(t, fresh)
	productionNames.Store(true)
	t.Cleanup(func() {
		productionNames.Store(false)
		db, err := sql.Open("pgx", base)
		if err == nil {
			_, _ = db.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
			_ = db.Close()
		}
	})
	return fresh
}

// SetURLForTest points the process at rawURL ("" for SQLite) until the test ends.
func SetURLForTest(t testing.TB, rawURL string) {
	t.Helper()
	swap := func(s *selection) {
		mu.Lock()
		defer mu.Unlock()
		if catalog != nil {
			_ = catalog.Close()
		}
		catalog, ready, selected = nil, false, s
	}
	mu.Lock()
	old := selected
	mu.Unlock()
	if old == nil {
		s := load()
		old = &s
	}
	swap(&selection{url: rawURL})
	t.Cleanup(func() { swap(old) })
}

// ReloadForTest rereads the environment, for tests of the configuration itself.
func ReloadForTest(t testing.TB) {
	t.Helper()
	mu.Lock()
	old := selected
	mu.Unlock()
	reset := func(s *selection) {
		mu.Lock()
		defer mu.Unlock()
		if catalog != nil {
			_ = catalog.Close()
		}
		catalog, ready, selected = nil, false, s
	}
	reset(nil)
	t.Cleanup(func() { reset(old) })
}

func dropStaleTestSchemas(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT nspname, COALESCE(obj_description(oid, 'pg_namespace'), '') FROM pg_namespace WHERE nspname LIKE 'kt\_%'`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var name, note string
		if err := rows.Scan(&name, &note); err != nil {
			rows.Close()
			return err
		}
		if p, ok := strings.CutPrefix(note, "path:"); ok && filepath.IsAbs(p) {
			if _, err := os.Stat(filepath.Dir(p)); os.IsNotExist(err) {
				stale = append(stale, name)
			}
		}
	}
	rows.Close()
	for _, name := range stale {
		if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+QuoteIdent(name)+` CASCADE; DELETE FROM kypost_catalog.stores WHERE schema='`+name+`'`); err != nil {
			return err
		}
	}
	return nil
}

// CatalogUnderForTest counts the catalog rows and the schemas whose path is under dir.
func CatalogUnderForTest(t testing.TB, dir string) (rows, schemas int) {
	t.Helper()
	cat, err := root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix := abs + string(filepath.Separator)
	if err := cat.QueryRow(`SELECT (SELECT count(*) FROM kypost_catalog.stores WHERE left(path, length(?))=?),
 (SELECT count(*) FROM pg_namespace WHERE left(COALESCE(obj_description(oid, 'pg_namespace'), ''), length(?))=?)`,
		prefix, prefix, "path:"+prefix, "path:"+prefix).Scan(&rows, &schemas); err != nil {
		t.Fatal(err)
	}
	return rows, schemas
}
