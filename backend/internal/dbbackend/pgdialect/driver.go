package pgdialect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"hash/fnv"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// The single writer. SQLite lets one writer in at a time and the store's correctness rests
// on that, so on Postgres every read-write transaction takes one transaction-scoped
// advisory lock right after BEGIN, and every write statement outside a transaction is run
// in its own transaction under the same lock. Writes are therefore serialised across every
// replica sharing the database; reads take no lock.
//
// KyPost keeps SQLite's one writer per file: each store schema derives its own key from
// its name (LockKeys), so writers on two stores never queue on each other. writerLockKey
// is the key of a connection with no search_path: the catalog and the whole-database
// copies of migrate-to-postgres and restore.
const writerLockKey int64 = 0x4b79_506f_0001 // "KyPo", 1

func init() { sql.Register("kypg", Driver{}) }

// Driver is the kypg database/sql driver: pgx's stdlib driver with Translate applied to
// every statement, and the writer lock.
type Driver struct{}

// Open refuses sql.Open("kypg", ...): stores borrow from the process's pool (NewPool,
// NewConnector), never from connections of their own.
func (Driver) Open(string) (driver.Conn, error) {
	return nil, errors.New("kypg: open a store through dbbackend")
}

// ErrBadURL is returned for a database URL pgx cannot parse. It never repeats the URL,
// which carries a password.
var ErrBadURL = errors.New("the database URL cannot be parsed (check KYPOST_DATABASE_URL)")

// ErrSchemaGone refuses a stale store writer after its schema was dropped.
var ErrSchemaGone = errors.New("database schema was removed; refusing a stale store writer")

// Config parses a postgres:// URL and applies the store's connection rules: a 5 s connect
// timeout, a writable primary (target_session_attrs=read-write unless the URL sets it),
// lock_timeout 5 s (SQLite's busy timeout) and idle_in_transaction_session_timeout 30 s
// unless the URL sets them, and timestamps read back in UTC.
func Config(rawURL string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return nil, ErrBadURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, ErrBadURL
	}
	q := u.Query()
	if !q.Has("connect_timeout") {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if !q.Has("target_session_attrs") {
		cfg.ValidateConnect = pgconn.ValidateConnectTargetSessionAttrsReadWrite
	}
	for k, v := range map[string]string{"lock_timeout": "5s", "idle_in_transaction_session_timeout": "30s"} {
		if _, set := cfg.RuntimeParams[k]; !set {
			cfg.RuntimeParams[k] = v
		}
	}
	return cfg, nil
}

// Describe names the server and database of a URL for logs: host and database only.
func Describe(rawURL string) string {
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return "(unparsable database URL)"
	}
	return cfg.Host + "/" + cfg.Database
}

// Unscoped is the search_path a pooled connection has between borrows: no schema, so a
// statement on a connection nobody scoped finds no table and fails.
const Unscoped = `""`

// ErrPoolWait is a borrow that found no free connection within PoolWait.
var ErrPoolWait = errors.New("database: no connection free in the pool")

// PoolWait bounds a borrow, as PgBouncer's query_wait_timeout would. A variable so tests can
// shorten it.
var PoolWait = 10 * time.Second

// resetSQL runs as a connection returns to the pool, off the caller's goroutine: it drops
// every session lock, setting (scope, read-only, lock_timeout), listener, cursor and temp
// table the borrower left. A connection it fails on is closed, never reused.
const resetSQL = `SELECT pg_advisory_unlock_all(); RESET ALL; UNLISTEN *; CLOSE ALL; DISCARD TEMP`

// NewPool is a process's one pool of server connections: size connections at most, every one
// opened at start and kept open (warm) when warm is set. Connections start Unscoped, and every
// release resets them (resetSQL). Each borrow scopes its connection (NewConnector).
func NewPool(rawURL string, size int, warm bool) (*pgxpool.Pool, error) {
	cfg, err := Config(rawURL)
	if err != nil {
		return nil, err
	}
	if _, set := cfg.RuntimeParams["search_path"]; set {
		return nil, errors.New("the database URL must not set search_path")
	}
	cfg.RuntimeParams["search_path"] = Unscoped
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, err
	}
	pc.ConnConfig = cfg
	pc.MaxConns = int32(size)
	if warm {
		pc.MinConns = int32(size)
	}
	if testing.Testing() {
		// A synctest bubble's clock runs years ahead: no lifetime expiry, so a borrow in a
		// bubble never closes a connection the process opened (puddle's WaitGroup).
		pc.MaxConnLifetime = 0
	}
	pc.AfterConnect = func(_ context.Context, c *pgx.Conn) error {
		c.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID, Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}})
		return nil
	}
	pc.AfterRelease = func(c *pgx.Conn) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := c.Exec(ctx, resetSQL)
		return err == nil
	}
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil || !warm {
		return p, err
	}
	fill(p, size)
	return p, nil
}

// fill opens a warm pool's connections before its first borrow, so a borrow never waits
// on a connect (nor, in a test, creates one inside a synctest bubble). A connect that fails
// is left to the pool's health check.
func fill(p *pgxpool.Pool, size int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := make(chan *pgxpool.Conn, size)
	var wg sync.WaitGroup
	for range size {
		wg.Go(func() {
			if c, err := p.Acquire(ctx); err == nil {
				held <- c
			}
		})
	}
	wg.Wait()
	close(held)
	for c := range held {
		c.Release()
	}
}

// NewConnector borrows from pool for one store. A readOnly store runs its transactions
// READ ONLY at REPEATABLE READ, a snapshot as a SQLite read transaction is, and never takes
// the writer lock. schema is the store's schema; "" is the catalog and whole-database work,
// scoped to public. Every borrow is scoped to schema and checked (scope).
func NewConnector(pool *pgxpool.Pool, readOnly bool, schema string) driver.Connector {
	return newConnector(pool, readOnly, false, schema)
}

// NewRowWriterConnector is for volumes.db accounts only. Account transactions
// lock changed rows and use SERIALIZABLE for set invariants; they must not hold
// the document writer lock while preparing a mailbox or recording a journal.
func NewRowWriterConnector(pool *pgxpool.Pool, schema string) driver.Connector {
	return newConnector(pool, false, true, schema)
}

func newConnector(pool *pgxpool.Pool, readOnly, rowWrites bool, schema string) driver.Connector {
	writer, _ := LockKeys(SearchPath(schema))
	return &connector{base: stdlib.GetPoolConnector(pool), txSlots: txSlotsFor(pool), readOnly: readOnly, lockKey: writer, rowWrites: rowWrites, schema: schema}
}

// Transactions may hold at most half of a pool, and fence sessions (state) a quarter, so a
// quarter is always left for single statements. A statement never waits on another borrow
// while it holds one, so a transaction that needs one more connection (a catalog lookup, a
// second store's read) always gets it: holders cannot take every connection and wait on each
// other. A transaction takes its slot before it borrows.
var txSlots sync.Map // *pgxpool.Pool -> chan struct{}

func txSlotsFor(pool *pgxpool.Pool) chan struct{} {
	ch, _ := txSlots.LoadOrStore(pool, make(chan struct{}, max(1, pool.Config().MaxConns/2)))
	return ch.(chan struct{})
}

// SearchPath is a store schema's search_path: the schema, then public (citext). The
// catalog's ("") is public.
func SearchPath(schema string) string {
	if schema == "" {
		return ""
	}
	return schema + ",public"
}

// LockKeys returns the writer lock key and the migration lock key for a search_path: the
// fixed keys for none, keys derived from it otherwise.
func LockKeys(schema string) (writer, migration int64) {
	if schema == "" {
		return writerLockKey, writerLockKey + 1
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(schema))
	k := int64(h.Sum64() &^ 1)
	return k, k + 1
}

type connector struct {
	base      driver.Connector
	txSlots   chan struct{}
	readOnly  bool
	lockKey   int64
	rowWrites bool
	schema    string
}

// Connect borrows nothing: the conn borrows a server connection at its first use (borrow) and
// returns it at Close, which database/sql calls after every statement or transaction (the
// handles keep no idle connections); a *sql.Conn session keeps it until it is closed.
func (c *connector) Connect(context.Context) (driver.Conn, error) {
	return &conn{c: c, readOnly: c.readOnly, lockKey: c.lockKey, rowWrites: c.rowWrites, requiredSchema: c.schema}, nil
}

// borrow takes a server connection from the pool and scopes it to the store, waiting at most
// PoolWait. A transaction takes its slot first (tx), and gives it back at its end.
func (c *conn) borrow(ctx context.Context, tx bool) error {
	if c.inner != nil {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, PoolWait)
	defer cancel()
	timedOut := func(err error) error {
		if wait.Err() != nil && ctx.Err() == nil {
			return ErrPoolWait
		}
		return err
	}
	if tx {
		select {
		case c.c.txSlots <- struct{}{}:
			c.txSlot = true
		case <-wait.Done():
			return timedOut(wait.Err())
		}
	}
	inner, err := c.c.base.Connect(wait)
	if err != nil {
		c.freeSlot()
		return timedOut(err)
	}
	sc := inner.(*stdlib.Conn)
	if err := scope(ctx, sc.Conn(), c.c.schema, c.c.readOnly); err != nil {
		_ = sc.Conn().Close(ctx) // never reused: the pool destroys a closed connection
		_ = sc.Close()
		c.freeSlot()
		return err
	}
	c.inner = sc
	return nil
}

func (c *conn) freeSlot() {
	if c.txSlot {
		c.txSlot = false
		<-c.c.txSlots
	}
}

// ErrScope is a borrowed connection whose scope is not the store's after scoping.
var ErrScope = errors.New("database: a pooled connection is not scoped to its store")

// scope sets a borrowed connection's search_path and read-only default for one store, in
// one round trip, and checks them: the effective schema must be the store's, so a missing
// schema cannot fall through to public. The release reset clears both.
func scope(ctx context.Context, c *pgx.Conn, schema string, readOnly bool) error {
	path, ro := SearchPath(schema), "off"
	if path == "" {
		path = "public"
	}
	if readOnly {
		ro = "on"
	}
	var b pgx.Batch
	b.Queue(`SELECT set_config('search_path', $1, false), set_config('default_transaction_read_only', $2, false)`, path, ro)
	b.Queue(`SELECT current_setting('search_path'), COALESCE(current_schema(), ''), current_setting('default_transaction_read_only')`)
	r := c.SendBatch(ctx, &b)
	var p1, r1, p2, cur, r2 string
	err := r.QueryRow().Scan(&p1, &r1)
	if err == nil {
		err = r.QueryRow().Scan(&p2, &cur, &r2)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	want := schema
	if want == "" {
		want = "public"
	}
	if p2 != path || r2 != ro || cur != want {
		if schema != "" && cur != schema && p2 == path {
			return ErrSchemaGone
		}
		return ErrScope
	}
	return nil
}

// A row-writer handle must not pass vol.NewTable's schema-writer driver check.
type rowDriver struct{ Driver }

func (c *connector) Driver() driver.Driver {
	if c.rowWrites {
		return rowDriver{}
	}
	return Driver{}
}

type conn struct {
	c              *connector
	inner          *stdlib.Conn // nil until borrow
	txSlot         bool
	readOnly       bool
	lockKey        int64
	inTx           bool
	rowWrites      bool
	requiredSchema string
}

func (c *conn) lock(ctx context.Context) error {
	if _, err := c.inner.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", []driver.NamedValue{{Ordinal: 1, Value: c.lockKey}}); err != nil {
		return err
	}
	if c.requiredSchema != "" {
		var present bool
		// Resolve the effective write namespace, not an MVCC catalog row:
		// REPEATABLE READ/SERIALIZABLE may still see a dropped pg_namespace row.
		// Keep this after the lock; a stale waiter must observe its new target.
		if err := c.inner.Conn().QueryRow(ctx, `SELECT COALESCE(current_schema()=$1,false)`, c.requiredSchema).Scan(&present); err != nil {
			return err
		}
		if !present {
			return ErrSchemaGone
		}
	}
	return nil
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, nil
}

func (c *conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, nil
}

func (c *conn) Close() error {
	c.freeSlot()
	if c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.beginTx(ctx, opts, true)
}

// beginTx begins a transaction; slot is false for a statement's own (autoWrap), which holds
// its connection no longer than a statement does.
func (c *conn) beginTx(ctx context.Context, opts driver.TxOptions, slot bool) (driver.Tx, error) {
	if err := c.borrow(ctx, slot); err != nil {
		return nil, err
	}
	if c.readOnly {
		opts.ReadOnly = true
		if sql.IsolationLevel(opts.Isolation) == sql.LevelDefault {
			opts.Isolation = driver.IsolationLevel(sql.LevelRepeatableRead)
		}
	} else if !c.rowWrites && sql.IsolationLevel(opts.Isolation) == sql.LevelDefault {
		// The writer lock is taken after BEGIN, so each statement must see what the
		// previous holder committed: READ COMMITTED, whatever the server's default is.
		// Row writers have no schema lock and retain their requested/server isolation.
		opts.Isolation = driver.IsolationLevel(sql.LevelReadCommitted)
	}
	tx, err := c.inner.BeginTx(ctx, opts)
	if err != nil {
		c.freeSlot()
		return nil, err
	}
	if !opts.ReadOnly && !c.rowWrites {
		if err := c.lock(ctx); err != nil {
			_ = tx.Rollback()
			c.freeSlot()
			return nil, err
		}
	}
	c.inTx = true
	return &txn{tx: tx, c: c}, nil
}

type txn struct {
	tx driver.Tx
	c  *conn
}

func (t *txn) Commit() error {
	t.c.inTx = false
	defer t.c.freeSlot()
	return t.tx.Commit()
}

func (t *txn) Rollback() error {
	t.c.inTx = false
	defer t.c.freeSlot()
	return t.tx.Rollback()
}

// autoWrap reports whether a statement must run in its own locked transaction: a write
// outside a transaction on a read-write pool.
func (c *conn) autoWrap(query string) bool {
	return !c.inTx && !c.readOnly && !c.rowWrites && !IsRead(query)
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.borrow(ctx, false); err != nil {
		return nil, err
	}
	run := func() (driver.Result, error) { return c.inner.ExecContext(ctx, Translate(query), args) }
	if stmts, params := Split(query); len(args) > 0 && len(stmts) > 1 {
		run = func() (driver.Result, error) {
			var res driver.Result
			for i, q := range stmts {
				part := args[:params[i]]
				args = args[params[i]:]
				for j := range part {
					part[j].Ordinal = j + 1
				}
				var err error
				if res, err = c.inner.ExecContext(ctx, Translate(q), part); err != nil {
					return nil, err
				}
			}
			return res, nil
		}
	}
	if !c.autoWrap(query) {
		return run()
	}
	tx, err := c.beginTx(ctx, driver.TxOptions{}, false)
	if err != nil {
		return nil, err
	}
	res, err := run()
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return res, tx.Commit()
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.borrow(ctx, false); err != nil {
		return nil, err
	}
	query = Translate(query)
	if !c.autoWrap(query) {
		return c.inner.QueryContext(ctx, query, args)
	}
	tx, err := c.beginTx(ctx, driver.TxOptions{}, false)
	if err != nil {
		return nil, err
	}
	r, err := c.inner.QueryContext(ctx, query, args)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &wrappedRows{Rows: r, tx: tx}, nil
}

// wrappedRows ends the transaction of an auto-wrapped write that returns rows (UPDATE ...
// RETURNING): commit when the rows run out, or at Close if the caller stopped early with
// no error; roll back on any error. A failed commit is reported by Next, so rows.Err and
// QueryRow's Scan see it.
type wrappedRows struct {
	driver.Rows
	tx   driver.Tx
	done bool
}

func (r *wrappedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	switch {
	case err == nil || r.done:
	case err == io.EOF:
		r.done = true
		if cerr := r.tx.Commit(); cerr != nil {
			return cerr
		}
	default:
		r.done = true
		_ = r.tx.Rollback()
	}
	return err
}

func (r *wrappedRows) Close() error {
	err := r.Rows.Close()
	if !r.done {
		r.done = true
		if err != nil {
			_ = r.tx.Rollback()
			return err
		}
		return r.tx.Commit()
	}
	return err
}

// Ping fails on a server that cannot take writes, so readiness fails while the database
// is a read-only standby and the pool reconnects to the new primary afterwards.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.borrow(ctx, false); err != nil {
		return err
	}
	if err := c.inner.Ping(ctx); err != nil {
		return err
	}
	if c.readOnly {
		return nil
	}
	var ro string
	if err := c.inner.Conn().QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil {
		return driver.ErrBadConn
	}
	if ro == "on" {
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) ResetSession(ctx context.Context) error {
	if c.inner == nil {
		return nil
	}
	return c.inner.ResetSession(ctx)
}

func (c *conn) IsValid() bool { return c.inner == nil || !c.inner.Conn().IsClosed() }

// CheckNamedValue passes every argument to pgx unchanged, as pgx's own driver does, except
// a bool: SQLite stores it as 1 or 0 and the store's flags are BIGINT columns, so it binds
// to a bigint parameter as 1 or 0 and to a boolean one (CASE WHEN ?) as itself.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if b, ok := nv.Value.(bool); ok {
		nv.Value = sqliteBool(b)
		return nil
	}
	return nil // pgx takes every other value as it is (stdlib.Conn.CheckNamedValue)
}

type sqliteBool bool

func (b sqliteBool) BoolValue() (pgtype.Bool, error) {
	return pgtype.Bool{Bool: bool(b), Valid: true}, nil
}

func (b sqliteBool) Int64Value() (pgtype.Int8, error) {
	if b {
		return pgtype.Int8{Int64: 1, Valid: true}, nil
	}
	return pgtype.Int8{Valid: true}, nil
}

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
