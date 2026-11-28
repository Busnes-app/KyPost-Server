package dbbackend

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One process, one pool. Every store handle (Open, Pool) and the catalog borrow server
// connections from a single pgxpool per database URL, of KYPOST_DATABASE_POOL_SIZE
// connections, all opened at start and kept open. A handle keeps no idle connection of its
// own, so neither a store, an org nor a client session holds one between statements. Each
// borrow is scoped to its store's schema and checked, and each release resets the session
// (pgdialect.NewPool, scope); docs/superpowers/specs/2026-11-20-tenancy-design.md,
// "Database connections".

// EnvPoolSize is the server connections one process holds on Postgres.
const EnvPoolSize = "KYPOST_DATABASE_POOL_SIZE"

const defaultPoolSize = 16

// ErrPoolWait is a statement that found no free connection within 10 s; callers answer 503.
var ErrPoolWait = pgdialect.ErrPoolWait

// pools is keyed by URL: a test may point the process at another database. Guarded by mu.
var pools = map[string]*pgxpool.Pool{}

// PoolSize is KYPOST_DATABASE_POOL_SIZE: 16 by default, from 4 to 1000.
func PoolSize() (int, error) {
	if n := poolSizeForTest.Load(); n > 0 {
		return int(n), nil
	}
	raw := os.Getenv(EnvPoolSize)
	if raw == "" {
		return defaultPoolSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 4 || n > 1000 {
		return 0, fmt.Errorf("%s must be a whole number from 4 to 1000", EnvPoolSize)
	}
	return n, nil
}

// Test hooks (SharedPoolForTest): a size below the configurable floor, and a cold pool.
var (
	poolSizeForTest atomic.Int32
	coldForTest     atomic.Bool
)

// sharedLocked is the pool for rawURL, opened on first use and warm.
func sharedLocked(rawURL string) (*pgxpool.Pool, error) {
	if p := pools[rawURL]; p != nil {
		return p, nil
	}
	n, err := PoolSize()
	if err != nil {
		return nil, err
	}
	p, err := pgdialect.NewPool(rawURL, n, !coldForTest.Load())
	if err != nil {
		return nil, err
	}
	pools[rawURL] = p
	return p, nil
}

// facadeLocked is a handle on schema over the shared pool. It keeps no idle connection.
func facadeLocked(rawURL, schema string, readOnly, rowWrites bool) (*sql.DB, error) {
	p, err := sharedLocked(rawURL)
	if err != nil {
		return nil, err
	}
	c := pgdialect.NewConnector(p, readOnly, schema)
	if rowWrites {
		c = pgdialect.NewRowWriterConnector(p, schema)
	}
	db := sql.OpenDB(c)
	db.SetMaxIdleConns(0)
	return db, nil
}
