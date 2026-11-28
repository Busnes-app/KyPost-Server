package dbbackend

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

const orgRowsDDL = `CREATE TABLE IF NOT EXISTS org_rows (org TEXT NOT NULL, n BIGINT NOT NULL)`

// serverConns counts the server connections carrying appName.
func serverConns(t *testing.T, appName string) int {
	t.Helper()
	admin, err := sql.Open("pgx", testBaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var n int
	if err := admin.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, appName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func testBaseURL(*testing.T) string { return os.Getenv(EnvTestURL) }

// Two orgs interleaving on one pooled connection never see or write each other's rows: every
// borrow is scoped to its store and every release resets the session. Both use the same
// statement texts, so a cached statement or plan from the other org would show here too.
func TestSharedConnectionIsolatesOrgs(t *testing.T) {
	SharedPoolForTest(t, 1, false)
	dir := t.TempDir()
	orgs := map[string]*sql.DB{}
	for _, org := range []string{"org-a", "org-b"} {
		db, err := Open(filepath.Join(dir, org, "state.db"), Options{Kind: "test", DDL: orgRowsDDL, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		orgs[org] = db
	}
	ctx := context.Background()
	pids := map[string]int{}
	for org, db := range orgs {
		var pid int
		if err := db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		pids[org] = pid
	}
	if pids["org-a"] != pids["org-b"] {
		t.Fatalf("the orgs did not share the one pooled connection: %v", pids)
	}
	const rounds = 150
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for org, db := range orgs {
		wg.Go(func() {
			errs <- interleave(ctx, db, org, rounds)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for org, db := range orgs {
		var mine, others int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE org=?), count(*) FILTER (WHERE org<>?) FROM org_rows`, org, org).Scan(&mine, &others); err != nil {
			t.Fatal(err)
		}
		if mine != 2*rounds || others != 0 {
			t.Fatalf("%s holds %d of its rows (want %d) and %d of the other org's", org, mine, 2*rounds, others)
		}
	}
}

// interleave writes and reads org's rows, outside and inside transactions, checking every
// read sees org's rows only.
func interleave(ctx context.Context, db *sql.DB, org string, rounds int) error {
	for i := range rounds {
		if _, err := db.ExecContext(ctx, `INSERT INTO org_rows (org, n) VALUES (?, ?)`, org, i); err != nil {
			return fmt.Errorf("%s insert: %w", org, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s begin: %w", org, err)
		}
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM org_rows WHERE org<>?`, org).Scan(&others); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s tx read: %w", org, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO org_rows (org, n) VALUES (?, ?)`, org, -i); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s tx insert: %w", org, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s commit: %w", org, err)
		}
		var outside int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_rows WHERE org<>?`, org).Scan(&outside); err != nil {
			return fmt.Errorf("%s read: %w", org, err)
		}
		if others != 0 || outside != 0 {
			return fmt.Errorf("%s saw %d and %d of another org's rows", org, others, outside)
		}
	}
	return nil
}

// A connection nobody scoped fails closed: between borrows it has no schema, so a store's
// table is not found. A handle on a schema that does not exist is refused on every borrow,
// never answered from public.
func TestUnscopedConnectionFailsClosed(t *testing.T) {
	SharedPoolForTest(t, 1, false)
	db, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{Kind: "test", DDL: orgRowsDDL, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO org_rows (org, n) VALUES ('a', 1)`); err != nil {
		t.Fatal(err)
	}
	raw0 := current().url
	mu.Lock()
	p := pools[raw0]
	mu.Unlock()
	raw, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	_ = raw.QueryRow(ctx, `SHOW search_path`).Scan(&path)
	_, err = raw.Exec(ctx, `SELECT count(*) FROM org_rows`)
	raw.Release()
	if err == nil || path != pgdialect.Unscoped {
		t.Fatalf("an unscoped borrow (search_path %q) read a store's table: %v", path, err)
	}
	gone, err := Pool("kt_no_such_schema", true)
	if err != nil {
		t.Fatal(err)
	}
	defer gone.Close()
	if err := gone.PingContext(ctx); !errors.Is(err, pgdialect.ErrSchemaGone) {
		t.Fatalf("a borrow for a missing schema: %v", err)
	}
}

// The pool stays at its size and warm while many orgs' stores and 1,000 idle client handles
// are open and 50 clients query at once: a handle or client holds no connection.
func TestSharedPoolBoundAndWarm(t *testing.T) {
	const size, orgs, idle, active = 4, 20, 1000, 50
	app := SharedPoolForTest(t, size, true)
	dir := t.TempDir()
	ctx := context.Background()
	paths := make([]string, orgs)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("org-%d", i), "state.db")
		db, err := Open(paths[i], Options{Kind: "test", DDL: orgRowsDDL, Create: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
	handles := make([]*sql.DB, idle)
	for i := range handles {
		db, err := Open(paths[i%orgs], Options{Kind: "test"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_rows`).Scan(new(int)); err != nil {
			t.Fatal(err)
		}
		handles[i] = db
	}
	peak := 0
	stop := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, active)
	for i := range active {
		wg.Go(func() {
			for time.Now().Before(stop) {
				if _, err := handles[i].ExecContext(ctx, `INSERT INTO org_rows (org, n) VALUES (?, ?)`, "x", i); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for time.Now().Before(stop) {
		peak = max(peak, serverConns(t, app))
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if peak > size {
		t.Fatalf("%d server connections with %d orgs and %d idle clients; the pool is %d", peak, orgs, idle, size)
	}
	// Warm: the pool keeps its connections open while idle, rather than closing them.
	deadline := time.Now().Add(10 * time.Second)
	n := serverConns(t, app)
	for n != size && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		n = serverConns(t, app)
	}
	if n != size {
		t.Fatalf("%d server connections idle, want the warm %d", n, size)
	}
}

// SharedPoolForTest points the process at a pool of its own on the test server, of size
// connections, kept warm if warm is set, until the test ends. Its connections carry the
// returned application_name, so pg_stat_activity counts them.
func SharedPoolForTest(t testing.TB, size int, warm bool) (appName string) {
	t.Helper()
	RequirePostgres(t)
	u, err := url.Parse(current().url)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	appName = "kt_pool_" + hex.EncodeToString(b[:])
	q := u.Query()
	q.Set("application_name", appName)
	u.RawQuery = q.Encode()
	poolSizeForTest.Store(int32(size))
	warmForTest.Store(warm)
	SetURLForTest(t, u.String())
	t.Cleanup(func() {
		poolSizeForTest.Store(0)
		warmForTest.Store(false)
		mu.Lock()
		p := pools[u.String()]
		delete(pools, u.String())
		mu.Unlock()
		if p != nil {
			p.Close()
		}
	})
	return appName
}
