package dbbackend

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigurationRefusals(t *testing.T) {
	const secret = "s3cretpw"
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "url")
	if err := os.WriteFile(good, []byte("postgres://kypost:"+secret+"@db.internal:5432/kypost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, url, file string
		want            error
	}{
		{"both", "postgres://kypost:" + secret + "@db.internal/kypost", good, ErrBoth},
		{"unparsable", "postgres://kypost:" + secret + "@db.internal:notaport/kypost", "", nil},
		{"keyword DSN", "host=db.internal user=kypost password=" + secret, "", nil},
		{"unreadable file", "", filepath.Join(dir, "missing"), ErrURLFile},
		{"empty file", "", empty, ErrURLFile},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvURL, c.url)
			t.Setenv(EnvURLFile, c.file)
			ReloadForTest(t)
			if !Postgres() {
				t.Fatal("a Postgres configuration error must not fall back to SQLite")
			}
			err := Check(context.Background())
			if err == nil {
				t.Fatal("startup check passed")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if c.name == "both" && (!strings.Contains(err.Error(), EnvURL) || !strings.Contains(err.Error(), EnvURLFile)) {
				t.Fatalf("%q does not name both variables", err)
			}
			if _, err2 := Open(filepath.Join(t.TempDir(), "state.db"), Options{Kind: "state", Create: true}); err2 == nil {
				t.Fatal("a store opened with a bad configuration")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("error %q leaks the URL", err)
			}
		})
	}
	t.Run("unset is SQLite", func(t *testing.T) {
		t.Setenv(EnvURL, "")
		t.Setenv(EnvURLFile, "")
		t.Setenv(EnvTestURL, "")
		ReloadForTest(t)
		if Postgres() || Name() != "sqlite" || Check(context.Background()) != nil {
			t.Fatal("no variable must mean SQLite")
		}
	})
}

// An unreachable server: neither the error nor the log line may carry the password or URL.
func TestConnectionErrorHidesURL(t *testing.T) {
	const raw = "postgres://nobody:hunter2pw@127.0.0.1:1/kypost?connect_timeout=2"
	t.Setenv(EnvURL, raw)
	t.Setenv(EnvURLFile, "")
	ReloadForTest(t)
	err := Check(context.Background())
	if err == nil {
		t.Fatal("connected to nothing")
	}
	if strings.Contains(err.Error(), "hunter2pw") || strings.Contains(err.Error(), raw) {
		t.Fatalf("error %q leaks the URL", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1/kypost") {
		t.Fatalf("error %q does not name the server", err)
	}
}

func TestSchemaNames(t *testing.T) {
	stateDir, configDir := t.TempDir(), t.TempDir()
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("CONFIG_DIR", configDir)
	productionNames.Store(true)
	defer productionNames.Store(false)
	id := strings.Repeat("u", 64)
	paths := []string{
		filepath.Join(stateDir, "state.db"),
		filepath.Join(stateDir, "users", id, "state.db"),
		filepath.Join(stateDir, "users", id, "mailbox", "mailbox.db"),
		filepath.Join(stateDir, "mailboxes", id, "state.db"),
		filepath.Join(stateDir, "mailboxes", id, "mailbox", "mailbox.db"),
		filepath.Join(stateDir, "receiving", "ingress.db"),
		filepath.Join(stateDir, "receiving", "cloudflare.db"),
		filepath.Join(configDir, "native-authority.db"),
	}
	seen := map[string]string{}
	for _, p := range paths {
		schema, rel, err := SchemaName(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(schema) > 63 || strings.Trim(schema, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
			t.Fatalf("%s: schema %q is not a short plain identifier", rel, schema)
		}
		if other, dup := seen[schema]; dup {
			t.Fatalf("%s and %s share schema %s", rel, other, schema)
		}
		seen[schema] = rel
		again, _, _ := SchemaName(p)
		if again != schema {
			t.Fatalf("%s: unstable name", rel)
		}
	}
	if s, _, _ := SchemaName(paths[0]); s != "kypost_state" {
		t.Fatalf("root state schema %q", s)
	}
	if _, _, err := SchemaName("/elsewhere/state.db"); err == nil && !testHook() {
		t.Fatal("a path outside the roots was named")
	}
}

func TestSingleWriterPerSchema(t *testing.T) {
	RequirePostgres(t)
	const ddl = `CREATE TABLE IF NOT EXISTS counter (id BIGINT PRIMARY KEY, n BIGINT NOT NULL);
INSERT INTO counter VALUES (1, 0) ON CONFLICT DO NOTHING`
	dir := t.TempDir()
	open := func(name string) *sql.DB {
		db, err := Open(filepath.Join(dir, name), Options{Kind: "test", DDL: ddl, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	a, b := open("a.db"), open("a.db")
	const each = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for _, db := range []*sql.DB{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				tx, err := db.Begin()
				if err != nil {
					errs <- err
					return
				}
				var n int
				if err = tx.QueryRow(`SELECT n FROM counter WHERE id=1`).Scan(&n); err == nil {
					_, err = tx.Exec(`UPDATE counter SET n=? WHERE id=1`, n+1)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := a.QueryRow(`SELECT n FROM counter WHERE id=1`).Scan(&n); err != nil || n != 2*each {
		t.Fatalf("counter %d, %v: writers on one schema did not serialise", n, err)
	}

	// A writer holding schema a's lock does not block a writer on schema c.
	tx, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	c := open("c.db")
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(`UPDATE counter SET n=n+1 WHERE id=1`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a writer on another schema waited for this one")
	}
	// And a second writer on a does wait.
	go func() {
		_, err := b.Exec(`UPDATE counter SET n=n+1 WHERE id=1`)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("a second writer on the same schema did not wait")
	case <-time.After(300 * time.Millisecond):
	}
	_ = tx.Rollback()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBoolArguments(t *testing.T) {
	RequirePostgres(t)
	db, err := Open(filepath.Join(t.TempDir(), "b.db"), Options{Kind: "test", DDL: `CREATE TABLE IF NOT EXISTS f (v BIGINT NOT NULL)`, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO f VALUES (?), (?)`, true, false); err != nil {
		t.Fatal(err)
	}
	var sum int
	var picked string
	if err := db.QueryRow(`SELECT sum(v), CASE WHEN ? THEN 'yes' ELSE 'no' END FROM f`, true).Scan(&sum, &picked); err != nil || sum != 1 || picked != "yes" {
		t.Fatalf("sum %d picked %q: %v", sum, picked, err)
	}
	var flag bool
	if err := db.QueryRow(`SELECT v FROM f WHERE v=1`).Scan(&flag); err != nil || !flag {
		t.Fatalf("scan bigint into bool: %v", err)
	}
}

func TestCatalogAndExists(t *testing.T) {
	RequirePostgres(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "x", "state.db")
	if ok, err := Exists(path); err != nil || ok {
		t.Fatalf("exists before open: %v %v", ok, err)
	}
	if _, err := Open(path, Options{Kind: "state"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open without create: %v", err)
	}
	db, err := Open(path, Options{Kind: "state", DDL: `CREATE TABLE IF NOT EXISTS t (a TEXT)`, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if ok, err := Exists(path); err != nil || !ok {
		t.Fatalf("exists after open: %v %v", ok, err)
	}
	if _, err := Open(path, Options{Kind: "mailbox"}); err == nil {
		t.Fatal("opened a state store as a mailbox")
	}
	if err := Remove(filepath.Join(dir, "users", "x")); err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(path); err != nil || ok {
		t.Fatalf("exists after remove: %v %v", ok, err)
	}
}

func TestLocaleRefusedBeforeWrite(t *testing.T) {
	RequirePostgres(t)
	base := current().url
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for name, create := range map[string]string{
		"kt_db_wrong_locale":   `ENCODING 'UTF8' LOCALE 'en_US.utf8' TEMPLATE template0`,
		"kt_db_wrong_encoding": `ENCODING 'SQL_ASCII' LOCALE 'C' TEMPLATE template0`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name)
			if _, err := admin.Exec(`CREATE DATABASE ` + name + ` ` + create); err != nil {
				t.Skipf("server cannot create it: %v", err)
			}
			defer func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`) }()
			parsed, err := url.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			parsed.Path = "/" + name
			u := parsed.String()
			SetURLForTest(t, u)
			if err := Check(context.Background()); err == nil || !strings.Contains(err.Error(), "C locale") {
				t.Fatalf("accepted: %v", err)
			}
			db, err := sql.Open("pgx", u)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM pg_namespace WHERE nspname='kypost_catalog'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("the refusal wrote the catalog: %d %v", n, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM pg_extension WHERE extname='citext'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("the refusal created citext: %d %v", n, err)
			}
		})
	}
}

// KYPOST_TEST_OLD_DATABASE_URL points at a Postgres older than 16, when one is at hand.
func TestOldServerRefused(t *testing.T) {
	old := os.Getenv("KYPOST_TEST_OLD_DATABASE_URL")
	if old == "" {
		t.Skip("set KYPOST_TEST_OLD_DATABASE_URL to a Postgres 15 server")
	}
	SetURLForTest(t, old)
	if err := Check(context.Background()); err == nil || !strings.Contains(err.Error(), "16 or newer") {
		t.Fatalf("an old server was accepted: %v", err)
	}
}
