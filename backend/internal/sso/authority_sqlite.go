package sso

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// The v3 runtime backend (docs/NATIVE_AUTHORITY_PER_ACCOUNT.md sections 2-5):
// native-authority.db beside the two stubs, one SQLite transaction per View
// or Update on a held handle. Nothing decoded outlives a call.

const nativeLifecycleFile = "sso-lifecycle.json"

type authorityFormat int

const (
	authorityFresh      authorityFormat = iota // no lifecycle, ledger or database
	authorityV2                                // v1/v2 JSON files, no database
	authorityV3                                // the database beside both stubs
	authorityFreshCrash                        // the database, the lifecycle absent or a stub, no ledger
)

const (
	fileAbsent = iota
	fileStub
	fileData
)

func stubFile(path string, stub map[string]any) (int, error) {
	ok, err := jsonStub(path, stub)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fileAbsent, nil
	case err != nil:
		return 0, ErrNativeMigration
	case ok:
		return fileStub, nil
	}
	return fileData, nil
}

// detectAuthority is the format table of section 5 plus the domain
// tombstone rule: any mix outside the table refuses with ErrNativeMigration.
func detectAuthority(dir string) (authorityFormat, error) {
	tombstone, data, err := legacyNativeDomain(dir)
	if err != nil || data {
		return 0, ErrNativeMigration
	}
	_, err = os.Lstat(filepath.Join(dir, NativeAuthorityFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, ErrNativeMigration
	}
	db := err == nil
	l, err := stubFile(filepath.Join(dir, nativeLifecycleFile), lifecycleStubV3)
	if err != nil {
		return 0, err
	}
	p, err := stubFile(filepath.Join(dir, nativeProvisioningFile), ledgerStubV3)
	if err != nil {
		return 0, err
	}
	format := authorityV2
	switch {
	case db && l == fileStub && p == fileStub:
		format = authorityV3
	case db && l != fileData && p == fileAbsent:
		format = authorityFreshCrash
	case !db && l == fileAbsent && p == fileAbsent:
		format = authorityFresh
	case db || l == fileStub || p == fileStub:
		return 0, ErrNativeMigration
	}
	if tombstone {
		for _, name := range []string{NativeDomainsFile, nativeProvisioningFile} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				return 0, ErrNativeMigration
			}
		}
	}
	return format, nil
}

// storeAuthority picks the backend from the files on every call. The runtime
// serves v3 and a fresh install and refuses anything else; an offline store
// (restore staging, migrate-native) reads and writes whichever it finds.
type storeAuthority struct{ s *LifecycleStore }

func (a storeAuthority) backend(ctx context.Context, write bool) (authorityStore, error) {
	dir := filepath.Dir(a.s.path)
	db := filepath.Join(dir, NativeAuthorityFile)
	if a.s.offline {
		// Staging and migration hold whatever format they were given; the
		// historical readers and v2NativeFormat judge the files themselves.
		if _, err := os.Lstat(db); errors.Is(err, os.ErrNotExist) {
			return jsonAuthority{a.s.path}, nil
		}
		return sqliteAuthority{path: db, offline: true}, nil
	}
	format, err := detectAuthority(dir)
	switch {
	case err != nil:
		return nil, err
	case format == authorityV2:
		return nil, ErrNativeMigration
	case format == authorityFresh && !write:
		// A fresh install reads as empty files. A first write racing this
		// read makes it refuse once, never read partial state.
		return jsonAuthority{a.s.path}, nil
	case write && format != authorityV3:
		// Under the caller's directory flock: the database, then the stubs.
		if err := createAuthority(ctx, dir, format == authorityFresh); err != nil {
			return nil, err
		}
	}
	return sqliteAuthority{path: db}, nil
}

func (a storeAuthority) View(ctx context.Context, fn func(authorityReader) error) error {
	b, err := a.backend(ctx, false)
	if err != nil {
		return err
	}
	return b.View(ctx, fn)
}

func (a storeAuthority) Update(ctx context.Context, fn func(authorityWriter) error) error {
	b, err := a.backend(ctx, true)
	if err != nil {
		return err
	}
	return b.Update(ctx, fn)
}

// createAuthority is the fresh-install write (database by rename, then the
// lifecycle stub, then the ledger stub) and the finish of one that crashed
// after the database.
func createAuthority(ctx context.Context, dir string, fresh bool) error {
	if fresh {
		rows, err := authorityV3Rows(lifecycleFile{}, nil)
		if err != nil {
			return err
		}
		if err := buildAuthority(ctx, dir, rows, nil); err != nil {
			return err
		}
	}
	return writeAuthorityStubs(dir)
}

// writeAuthorityStubs writes the lifecycle stub, then the ledger stub last.
func writeAuthorityStubs(dir string) error {
	for _, stub := range []struct {
		name string
		doc  map[string]any
	}{{nativeLifecycleFile, lifecycleStubV3}, {nativeProvisioningFile, ledgerStubV3}} {
		path := filepath.Join(dir, stub.name)
		if state, err := stubFile(path, stub.doc); err != nil || state == fileStub {
			if err != nil {
				return err
			}
			continue
		}
		if err := fsutil.PersistJSONFile(path, stub.doc); err != nil {
			return err
		}
	}
	return nil
}

// privateRegular is the open-time file rule: a regular file (not a symlink)
// that only its owner can read or write.
func privateRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrNativeMigration
	}
	return info, nil
}

// authorityErr maps driver errors to the provisioning refusal (section 4).
func authorityErr(err error) error {
	if err == nil || errors.Is(err, ErrNativeMigration) || errors.Is(err, ErrNativeProvisioning) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrNativeProvisioning, err)
}

// The runtime DSN. It never uses immutable=1 (that ignores the WAL) and the
// store never runs ANALYZE or PRAGMA optimize (sqlite_stat1 breaks the exact
// schema rule). journal_mode(WAL) converts a restored backup copy (VACUUM
// INTO writes rollback-journal mode) on its first, offline, open.
const authorityDSN = "?mode=rw&_defensive=1&_pragma=journal_mode(WAL)&_pragma=trusted_schema(0)&_pragma=recursive_triggers(1)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)&_pragma=busy_timeout(10000)"

// verifiedConnector checks every physical connection when it opens: the
// pragmas, defensive mode, WAL, user_version 3 and the exact schema.
type verifiedConnector struct{ driver.Connector }

func (c verifiedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sc, ok := conn.(sqliteConn)
	if !ok {
		_ = conn.Close()
		return nil, ErrNativeMigration
	}
	if err := verifyAuthorityConn(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &stmtConn{sqliteConn: sc, stmts: map[string]driver.Stmt{}}, nil
}

// sqliteConn is the driver connection with the interfaces database/sql uses.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// maxConnStmts bounds the statements kept per connection; the store's
// queries are fixed strings, about 20.
const maxConnStmts = 64

// stmtConn prepares each query once per physical connection and finalizes
// the statements when the connection closes. SQLite re-prepares a statement
// itself after a schema change, and every run of it reads in the caller's
// transaction, so only the parse is reused, never a value.
type stmtConn struct {
	sqliteConn
	stmts map[string]driver.Stmt
}

// QueryContext takes the query's statement out of the cache until its rows
// close, so a nested run of the same query prepares its own.
func (c *stmtConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.stmts[query]
	delete(c.stmts, query)
	if s == nil {
		var err error
		if s, err = c.PrepareContext(ctx, query); err != nil {
			return nil, err
		}
	}
	q, ok := s.(driver.StmtQueryContext)
	if !ok {
		_ = s.Close()
		return nil, ErrNativeMigration
	}
	rows, err := q.QueryContext(ctx, args)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return &stmtRows{Rows: rows, c: c, query: query, s: s}, nil
}

func (c *stmtConn) Close() error {
	for _, s := range c.stmts {
		_ = s.Close()
	}
	c.stmts = nil
	return c.sqliteConn.Close()
}

// stmtRows returns its statement to the cache when it closes.
type stmtRows struct {
	driver.Rows
	c     *stmtConn
	query string
	s     driver.Stmt
}

func (r *stmtRows) Close() error {
	err := r.Rows.Close()
	if r.s == nil {
		return err
	}
	if err == nil && r.c.stmts != nil && r.c.stmts[r.query] == nil && len(r.c.stmts) < maxConnStmts {
		r.c.stmts[r.query] = r.s
	} else {
		_ = r.s.Close()
	}
	r.s = nil
	return err
}

func driverRows(ctx context.Context, conn driver.Conn, query string) ([][]any, error) {
	q, ok := conn.(driver.QueryerContext)
	if !ok {
		return nil, ErrNativeMigration
	}
	rows, err := q.QueryContext(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for {
		dest := make([]driver.Value, len(rows.Columns()))
		if err := rows.Next(dest); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		row := make([]any, len(dest))
		for i, v := range dest {
			row[i] = v
		}
		out = append(out, row)
	}
}

func verifyAuthorityConn(ctx context.Context, conn driver.Conn) error {
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		return ErrNativeMigration
	}
	// Defensive mode leaves writable_schema off whatever is asked.
	if _, err := exec.ExecContext(ctx, "PRAGMA writable_schema=ON", nil); err != nil {
		return fmt.Errorf("%w: %w", ErrNativeMigration, err)
	}
	want := map[string]any{
		"SELECT trusted_schema FROM pragma_trusted_schema":         int64(0),
		"SELECT recursive_triggers FROM pragma_recursive_triggers": int64(1),
		"SELECT foreign_keys FROM pragma_foreign_keys":             int64(1),
		"SELECT synchronous FROM pragma_synchronous":               int64(2),
		"SELECT user_version FROM pragma_user_version":             int64(3),
		"PRAGMA writable_schema":                                   int64(0),
		"PRAGMA journal_mode":                                      "wal",
	}
	for query, value := range want {
		rows, err := driverRows(ctx, conn, query)
		if err != nil || len(rows) != 1 || rows[0][0] != value {
			return ErrNativeMigration
		}
	}
	rows, err := driverRows(ctx, conn, schemaQuery)
	if err != nil {
		return ErrNativeMigration
	}
	reference, err := referenceSchemaV3()
	if err != nil || !slices.Equal(schemaOf(rows), reference) {
		return ErrNativeMigration
	}
	return nil
}

// openAuthorityDB opens a verified pool on an existing database file.
func openAuthorityDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	base, err := sqlite.NewConnector((&url.URL{Scheme: "file", Path: abs}).String() + authorityDSN)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(verifiedConnector{base})
	db.SetMaxIdleConns(4)
	// Open one connection now, so a refused file refuses here.
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, authorityErr(err)
	}
	return db, nil
}

// authorityHandle is a held pool (owner decision Q1) and the identity of the
// file it opened.
type authorityHandle struct {
	db      *sql.DB
	info    os.FileInfo
	refs    int
	dropped bool
}

// ponytail: production holds one handle (one config dir). Tests and restore
// staging open more; past maxAuthorityHandles idle ones are closed.
const maxAuthorityHandles = 8

var authorityHandles = struct {
	sync.Mutex
	m map[string]*authorityHandle
}{m: map[string]*authorityHandle{}}

func acquireAuthority(path string) (*authorityHandle, error) {
	info, err := privateRegular(path)
	if err != nil {
		return nil, err
	}
	authorityHandles.Lock()
	defer authorityHandles.Unlock()
	if h := authorityHandles.m[path]; h != nil {
		if os.SameFile(h.info, info) {
			h.refs++
			return h, nil
		}
		dropAuthorityLocked(path, h)
	}
	db, err := openAuthorityDB(path)
	if err != nil {
		return nil, err
	}
	if len(authorityHandles.m) >= maxAuthorityHandles {
		for other, h := range authorityHandles.m {
			dropAuthorityLocked(other, h)
		}
	}
	h := &authorityHandle{db: db, info: info, refs: 1}
	authorityHandles.m[path] = h
	return h, nil
}

func releaseAuthority(path string, h *authorityHandle, drop bool) {
	authorityHandles.Lock()
	defer authorityHandles.Unlock()
	h.refs--
	if drop {
		dropAuthorityLocked(path, h)
	}
	closeIdleAuthorityLocked(h)
}

// dropAuthorityLocked forgets a handle; it closes once no call uses it.
func dropAuthorityLocked(path string, h *authorityHandle) {
	if authorityHandles.m[path] == h {
		delete(authorityHandles.m, path)
	}
	h.dropped = true
	closeIdleAuthorityLocked(h)
}

func closeIdleAuthorityLocked(h *authorityHandle) {
	if h.dropped && h.refs == 0 && h.db != nil {
		_ = h.db.Close()
		h.db = nil
	}
}

// dropAuthorityHandle closes this process's handle on path, so the file can
// be replaced (migration) or removed (tests) without a stale pool.
func dropAuthorityHandle(path string) {
	authorityHandles.Lock()
	defer authorityHandles.Unlock()
	if h := authorityHandles.m[path]; h != nil {
		dropAuthorityLocked(path, h)
	}
}

// sqliteAuthority is one database. View is BEGIN (deferred) on a pooled
// connection; Update is BEGIN IMMEDIATE, the single writer, under the
// caller's directory flock. offline (restore staging) checkpoints and closes
// after every write, so no WAL is left for the historical opener.
type sqliteAuthority struct {
	path    string
	offline bool
}

func (a sqliteAuthority) View(ctx context.Context, fn func(authorityReader) error) error {
	_, err := a.run(ctx, false, func(t *sqliteTx) error { return fn(t) })
	return err
}

// Update commits every put in one transaction, then writes the routes of
// routes_pending inactive when the writer asked (MarkRoutesPending). A route
// failure leaves the commit and its routes_pending rows: ErrNativeRoutesPending.
func (a sqliteAuthority) Update(ctx context.Context, fn func(authorityWriter) error) error {
	t, err := a.run(ctx, true, func(t *sqliteTx) error { return fn(t) })
	if err != nil || !t.routes {
		return err
	}
	return a.flushRoutes(ctx)
}

func (a sqliteAuthority) run(ctx context.Context, write bool, fn func(*sqliteTx) error) (*sqliteTx, error) {
	h, err := acquireAuthority(a.path)
	if err != nil {
		return nil, err
	}
	drop := write && a.offline
	defer func() { releaseAuthority(a.path, h, drop) }()
	c, err := h.db.Conn(ctx)
	if err != nil {
		drop = true
		return nil, authorityErr(err)
	}
	defer c.Close()
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err := c.ExecContext(ctx, begin); err != nil {
		return nil, authorityErr(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	t := &sqliteTx{ctx: ctx, c: c}
	if err := t.readMeta(); err != nil {
		return nil, err
	}
	if err := fn(t); err != nil {
		return nil, err
	}
	if write {
		if err := t.flush(); err != nil {
			return nil, authorityErr(err)
		}
	}
	// Section 4: the rows came from the file this handle opened.
	if info, err := os.Lstat(a.path); err != nil || !os.SameFile(h.info, info) {
		drop = true
		return nil, ErrNativeMigration
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, authorityErr(err)
	}
	committed = true
	if drop {
		if _, err := c.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return nil, authorityErr(err)
		}
	}
	return t, nil
}

// flushRoutes writes every routes_pending route inactive, then clears the
// rows it wrote. The caller still holds the directory flock.
func (a sqliteAuthority) flushRoutes(ctx context.Context) error {
	r := authorityRows{}
	if _, err := a.run(ctx, false, func(t *sqliteTx) error {
		if err := queryRows(ctx, t.c, "routes", "SELECT address, generation FROM routes_pending", r); err != nil {
			return err
		}
		return queryRows(ctx, t.c, "root", "SELECT state_root FROM mailboxes LIMIT 1", r)
	}); err != nil || len(r["routes"]) == 0 || len(r["root"]) == 0 {
		return authorityErr(err)
	}
	d := &rowDecoder{}
	pending := map[string]int64{}
	for _, row := range r["routes"] {
		pending[d.str(row[0])] = d.int(row[1])
	}
	if root := d.str(r["root"][0][0]); d.err != nil {
		return d.err
	} else if err := deactivateRoutes(root, pending); err != nil {
		return err
	}
	_, err := a.run(ctx, true, func(t *sqliteTx) error {
		for address, generation := range pending {
			if _, err := t.c.ExecContext(ctx, "DELETE FROM routes_pending WHERE address = ? AND generation = ?", address, generation); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// sqliteTx is one transaction. Point reads always query; the whole lifecycle
// and ledger are read at most once, for the cold paths and the write diff.
type sqliteTx struct {
	ctx  context.Context
	c    *sql.Conn
	meta authorityRows

	life   *lifecycleFile
	ledger *nativeLedger
	loaded bool // ledger read (nil when uninitialized)

	// Update puts, written by flush as one diff against the stored rows.
	init      bool
	lifePut   *lifecycleFile
	accounts  map[string]nativeLedgerAccount
	mailboxes map[string]nativeLedgerMailbox
	addresses map[string]nativeLedgerAddress
	routes    bool
}

func (t *sqliteTx) readMeta() error {
	t.meta = authorityRows{}
	if err := queryRows(t.ctx, t.c, "meta", "SELECT key, value FROM meta", t.meta); err != nil {
		return authorityErr(err)
	}
	if t.metaValue("format") != "3" {
		return ErrNativeMigration
	}
	if v := t.metaValue("initialized"); v != "0" && v != "1" {
		return ErrNativeProvisioning
	}
	return nil
}

func (t *sqliteTx) metaValue(key string) string {
	for _, row := range t.meta["meta"] {
		if row[0] == key {
			s, _ := row[1].(string)
			return s
		}
	}
	return ""
}

// row runs a point query; more than one row is a broken key.
func (t *sqliteTx) row(query string, args ...any) ([]any, bool, error) {
	r := authorityRows{}
	if err := queryRows(t.ctx, t.c, "row", query, r, args...); err != nil {
		return nil, false, authorityErr(err)
	}
	switch len(r["row"]) {
	case 0:
		return nil, false, nil
	case 1:
		return r["row"][0], true, nil
	}
	return nil, false, ErrNativeProvisioning
}

func (t *sqliteTx) Initialized() (bool, error) {
	return t.metaValue("initialized") == "1" || t.init || t.lifePut != nil && t.lifePut.NativeProvisioningInitialized, nil
}

func (t *sqliteTx) Directory(issuer, subject string) (DirectoryState, bool, error) {
	if t.lifePut != nil {
		d, ok := t.lifePut.Directory[directoryKey(issuer, subject)]
		return d, ok, nil
	}
	row, ok, err := t.row("SELECT * FROM directory WHERE issuer = ? AND subject = ?", issuer, subject)
	if err != nil || !ok {
		return DirectoryState{}, false, err
	}
	d := &rowDecoder{}
	s := d.directory(row)
	return s, true, d.err
}

const accountQuery = `SELECT a.revision, a.digest, a.desired_active, a.status, a.failure, a.primary_mailbox, a.legacy_mixed_use,
  m.kind, m.issuer, m.subject, m.state_root, m.message_bytes, m.payload_bytes, m.records, m.source,
  (SELECT address FROM addresses WHERE mailbox = a.primary_mailbox AND kind = 'primary')
FROM accounts a LEFT JOIN mailboxes m ON m.id = a.primary_mailbox WHERE a.issuer = ? AND a.subject = ?`

// account reads the subject's account row, primary mailbox and primary address.
func (t *sqliteTx) account(issuer, subject string) (NativeAssignment, nativeLedgerAccount, bool, error) {
	row, ok, err := t.row(accountQuery, issuer, subject)
	if err != nil || !ok {
		return NativeAssignment{}, nativeLedgerAccount{}, false, err
	}
	if initialized, _ := t.Initialized(); !initialized {
		return NativeAssignment{}, nativeLedgerAccount{}, false, ErrNativeProvisioning // ledger rows behind an unset flag
	}
	d := &rowDecoder{}
	a := nativeLedgerAccount{Revision: d.int(row[0]), Digest: d.str(row[1]), DesiredActive: d.bool(row[2]), Status: d.str(row[3]), Failure: d.str(row[4]), PrimaryMailbox: d.str(row[5]), LegacyMixedUse: d.bool(row[6])}
	if row[7] != "primary" || row[8] != issuer || row[9] != subject {
		return NativeAssignment{}, nativeLedgerAccount{}, false, ErrNativeProvisioning
	}
	n := NativeAssignment{Owner: mailbox.Owner{Issuer: issuer, Subject: subject, Mailbox: a.PrimaryMailbox}, StateRoot: d.str(row[10]),
		Limits:   mailbox.Limits{MessageBytes: d.int(row[11]), PayloadBytes: d.int(row[12]), Records: int(d.int(row[13]))},
		Revision: a.Revision, Digest: a.Digest, DesiredActive: a.DesiredActive, Status: a.Status, Failure: a.Failure, Source: d.str(row[14]), LegacyMixedUse: a.LegacyMixedUse}
	if row[15] != nil {
		n.Address = d.str(row[15])
	}
	return n, a, true, d.err
}

func (t *sqliteTx) Account(issuer, subject string) (NativeAssignment, bool, error) {
	a, _, ok, err := t.account(issuer, subject)
	return a, ok, err
}

// Mailbox is nativeAssignments.mailbox on rows.
func (t *sqliteTx) Mailbox(id string) (NativeAssignment, nativeLedgerMailbox, bool, error) {
	row, ok, err := t.row("SELECT issuer, subject, kind, state, state_root, message_bytes, payload_bytes, records, source FROM mailboxes WHERE id = ?", id)
	if err != nil || !ok {
		return NativeAssignment{}, nativeLedgerMailbox{}, false, err
	}
	d := &rowDecoder{}
	m := nativeLedgerMailbox{Kind: d.str(row[2]), State: d.str(row[3]), StateRoot: d.str(row[4]), Limits: mailbox.Limits{MessageBytes: d.int(row[5]), PayloadBytes: d.int(row[6]), Records: int(d.int(row[7]))}, Source: d.str(row[8])}
	m.Owner.Issuer, m.Owner.Subject = d.str(row[0]), d.str(row[1])
	if d.err != nil {
		return NativeAssignment{}, nativeLedgerMailbox{}, false, d.err
	}
	a, ok, err := t.Account(m.Owner.Issuer, m.Owner.Subject)
	if err != nil || !ok || m.Kind == "primary" && a.Owner.Mailbox != id {
		return NativeAssignment{}, m, false, err
	}
	if m.Kind == "primary" {
		return a, m, true, nil
	}
	a.user = a.Owner.Mailbox
	a.Owner.Mailbox, a.StateRoot, a.Limits, a.Source, a.extra = id, m.StateRoot, m.Limits, m.Source, true
	address, ok, err := t.row("SELECT address FROM addresses WHERE mailbox = ? AND kind = 'primary'", id)
	if err != nil || !ok {
		return NativeAssignment{}, m, false, cmp.Or(err, ErrNativeProvisioning) // an extra mailbox has its own primary address
	}
	a.Address = d.str(address[0])
	return a, m, true, d.err
}

func (t *sqliteTx) Address(address string) (nativeLedgerAddress, bool, error) {
	row, ok, err := t.row("SELECT mailbox, kind, state, generation FROM addresses WHERE address = ?", address)
	if err != nil || !ok {
		return nativeLedgerAddress{}, false, err
	}
	d := &rowDecoder{}
	x := nativeLedgerAddress{Mailbox: d.str(row[0]), Kind: d.str(row[1]), State: d.str(row[2]), Generation: d.int(row[3])}
	r := authorityRows{}
	if err := queryRows(t.ctx, t.c, "history", "SELECT mailbox, generation FROM address_history WHERE address = ? ORDER BY generation", r, address); err != nil {
		return nativeLedgerAddress{}, false, authorityErr(err)
	}
	for _, h := range r["history"] {
		x.History = append(x.History, nativeAddressHistory{Mailbox: d.str(h[0]), Generation: d.int(h[1])})
	}
	if d.err != nil || !x.validHistory() {
		return nativeLedgerAddress{}, false, ErrNativeProvisioning
	}
	return x, true, nil
}

var (
	lifecycleTablesV3 = []string{"directory", "directory_primary_emails", "directory_events", "logouts", "sign_ons", "release_floors", "recovery_floors", "recovery_barriers", "lifecycle_records"}
	ledgerTablesV3    = []string{"mailboxes", "accounts", "addresses", "address_history", "routes_pending"}
)

// read decodes meta plus tables as authorityRows.decode does.
func (t *sqliteTx) read(tables []string) (lifecycleFile, *nativeLedger, error) {
	r := authorityRows{"meta": t.meta["meta"]}
	for _, table := range tables {
		if err := readTable(t.ctx, t.c, table, r); err != nil {
			return lifecycleFile{}, nil, authorityErr(err)
		}
	}
	return r.decode()
}

// storedLifecycle is the committed lifecycle, read once.
func (t *sqliteTx) storedLifecycle() (lifecycleFile, error) {
	if t.life == nil {
		life, _, err := t.read(lifecycleTablesV3)
		if err != nil {
			return lifecycleFile{}, err
		}
		t.life = &life
	}
	return *t.life, nil
}

// storedLedger is the committed ledger, read once; nil before initialization.
func (t *sqliteTx) storedLedger() (*nativeLedger, error) {
	if !t.loaded {
		_, l, err := t.read(ledgerTablesV3)
		if err != nil {
			return nil, err
		}
		t.ledger, t.loaded = l, true
	}
	return t.ledger, nil
}

func (t *sqliteTx) Lifecycle() (lifecycleFile, error) {
	if t.lifePut != nil {
		return t.lifePut.clone(), nil
	}
	life, err := t.storedLifecycle()
	if err != nil {
		return lifecycleFile{}, err
	}
	life = life.clone()
	life.NativeProvisioningInitialized, _ = t.Initialized()
	return life, nil
}

func (t *sqliteTx) Logouts() (map[string]LogoutEvent, error) {
	if t.lifePut != nil {
		return maps.Clone(t.lifePut.Logouts), nil
	}
	life, _, err := t.read([]string{"logouts"})
	return life.Logouts, err
}

func cloneLedger(l nativeLedger) nativeLedger {
	l.Accounts, l.Mailboxes, l.Addresses = maps.Clone(l.Accounts), maps.Clone(l.Mailboxes), maps.Clone(l.Addresses)
	return l
}

// Snapshot reads every row and validates the whole ledger, as the JSON
// backend's loadNative did; the maps are the caller's.
func (t *sqliteTx) Snapshot() (nativeAssignments, map[string]DirectoryState, error) {
	life, err := t.Lifecycle()
	if err != nil {
		return nativeAssignments{}, nil, err
	}
	l, err := t.storedLedger()
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}}
	if err != nil || l == nil {
		return f, life.Directory, err
	}
	f, _, err = validateNativeLedger(cloneLedger(*l))
	return f, life.Directory, err
}

// Check is the whole-store validation: quick_check, foreign keys, and every
// row decoded and validated.
func (t *sqliteTx) Check() error {
	r := authorityRows{}
	if err := queryRows(t.ctx, t.c, "quick", "PRAGMA quick_check", r); err != nil || len(r["quick"]) != 1 || r["quick"][0][0] != "ok" {
		return cmp.Or(authorityErr(err), ErrNativeProvisioning)
	}
	if err := queryRows(t.ctx, t.c, "fk", "PRAGMA foreign_key_check", r); err != nil || len(r["fk"]) != 0 {
		return cmp.Or(authorityErr(err), ErrNativeProvisioning)
	}
	_, _, err := t.Snapshot()
	return err
}

func (t *sqliteTx) Change() (int64, error) {
	n, err := strconv.ParseInt(t.metaValue("change"), 10, 64)
	if err != nil || n < 0 {
		return 0, ErrNativeProvisioning
	}
	return n, nil
}

// OwnerMailboxes reads one owning account's mailboxes and their addresses
// (without history) by the mailboxes_owner and addresses_mailbox indexes.
func (t *sqliteTx) OwnerMailboxes(userID string) (nativeAssignments, error) {
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}, stored: nativeLedger{Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}}
	owner, ok, err := t.row("SELECT issuer, subject FROM mailboxes WHERE id = ? AND kind = 'primary'", userID)
	if err != nil || !ok {
		return f, err
	}
	d := &rowDecoder{}
	issuer, subject := d.str(owner[0]), d.str(owner[1])
	a, account, ok, err := t.account(issuer, subject)
	if err != nil || !ok || d.err != nil {
		return f, cmp.Or(err, d.err)
	}
	key := directoryKey(issuer, subject)
	f.Accounts[key], f.stored.Accounts[key] = a, account
	r := authorityRows{}
	if err := queryRows(t.ctx, t.c, "mailboxes", "SELECT id, issuer, subject, kind, state, state_root, message_bytes, payload_bytes, records, source FROM mailboxes WHERE issuer = ? AND subject = ?", r, issuer, subject); err != nil {
		return f, authorityErr(err)
	}
	if err := queryRows(t.ctx, t.c, "addresses", "SELECT address, mailbox, kind, state, generation FROM addresses WHERE mailbox IN (SELECT id FROM mailboxes WHERE issuer = ? AND subject = ?)", r, issuer, subject); err != nil {
		return f, authorityErr(err)
	}
	for _, row := range r["mailboxes"] {
		m := nativeLedgerMailbox{Kind: d.str(row[3]), State: d.str(row[4]), StateRoot: d.str(row[5]), Limits: mailbox.Limits{MessageBytes: d.int(row[6]), PayloadBytes: d.int(row[7]), Records: int(d.int(row[8]))}, Source: d.str(row[9])}
		m.Owner.Issuer, m.Owner.Subject = d.str(row[1]), d.str(row[2])
		f.stored.Mailboxes[d.str(row[0])] = m
	}
	for _, row := range r["addresses"] {
		f.stored.Addresses[d.str(row[0])] = nativeLedgerAddress{Mailbox: d.str(row[1]), Kind: d.str(row[2]), State: d.str(row[3]), Generation: d.int(row[4])}
	}
	return f, d.err
}

func (t *sqliteTx) SetInitialized() error {
	t.init = true
	return nil
}

func (t *sqliteTx) pending() error {
	if !t.init {
		return ErrNativeProvisioning // a ledger write starts with SetInitialized
	}
	if t.accounts == nil {
		t.accounts, t.mailboxes, t.addresses = map[string]nativeLedgerAccount{}, map[string]nativeLedgerMailbox{}, map[string]nativeLedgerAddress{}
	}
	return nil
}

func (t *sqliteTx) PutAccount(key string, a nativeLedgerAccount) error {
	err := t.pending()
	if err == nil {
		t.accounts[key] = a
	}
	return err
}

func (t *sqliteTx) PutMailbox(id string, m nativeLedgerMailbox) error {
	err := t.pending()
	if err == nil {
		t.mailboxes[id] = m
	}
	return err
}

func (t *sqliteTx) PutAddress(address string, x nativeLedgerAddress) error {
	err := t.pending()
	if err == nil {
		t.addresses[address] = x
	}
	return err
}

func (t *sqliteTx) PutLifecycle(l lifecycleFile) error {
	c := l.clone()
	t.lifePut = &c
	return nil
}

// MarkRoutesPending asks Update to write routes_pending inactive after the
// commit. The rows themselves follow every committed address change.
func (t *sqliteTx) MarkRoutesPending(map[string]int64) error {
	t.routes = true
	return nil
}

// authorityColumns names each table's columns in schema order; the first
// authorityKeys[table] (default 1) are its primary key.
var (
	authorityColumns = map[string][]string{
		"directory":                {"issuer", "subject", "revision", "digest", "active", "event_id", "revoked_before", "resource"},
		"directory_primary_emails": {"fold", "issuer", "subject"},
		"accounts":                 {"issuer", "subject", "revision", "digest", "desired_active", "status", "failure", "primary_mailbox", "legacy_mixed_use"},
		"mailboxes":                {"id", "issuer", "subject", "kind", "state", "state_root", "message_bytes", "payload_bytes", "records", "source"},
		"addresses":                {"address", "mailbox", "kind", "state", "generation"},
		"address_history":          {"address", "generation", "mailbox"},
		"routes_pending":           {"address", "generation"},
		"directory_events":         {"key", "value", "expires"},
		"logouts":                  {"key", "value", "expires"},
		"sign_ons":                 {"key", "expires"},
		"release_floors":           {"key", "value"},
		"recovery_floors":          {"key", "value"},
		"recovery_barriers":        {"key", "value"},
		"lifecycle_records":        {"name", "json"},
	}
	authorityKeys = map[string]int{"directory": 2, "directory_primary_emails": 3, "accounts": 2, "address_history": 2}
	// Rows that are never deleted (the schema's triggers say the same).
	authorityKept = map[string]bool{"accounts": true, "mailboxes": true, "addresses": true, "address_history": true}
)

func compareRows(a, b []any) int {
	for i := range min(len(a), len(b)) {
		var c int
		switch x := a[i].(type) {
		case string:
			y, _ := b[i].(string)
			c = cmp.Compare(x, y)
		case int64:
			y, _ := b[i].(int64)
			c = cmp.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// writeDiff writes new over old table by table, in authorityTablesV3 order,
// inserts in key order (history generations only increase). It never uses
// REPLACE: a changed row is INSERT ... ON CONFLICT DO UPDATE, so the update
// triggers fire. It returns the changed rows by table (nil: deleted).
func (t *sqliteTx) writeDiff(old, new authorityRows, tables []string) (map[string][][]any, error) {
	changed := map[string][][]any{}
	for _, table := range authorityTablesV3 {
		if !slices.Contains(tables, table) {
			continue
		}
		columns, keys := authorityColumns[table], cmp.Or(authorityKeys[table], 1)
		index := func(rows [][]any) map[string][]any {
			m := make(map[string][]any, len(rows))
			for _, row := range rows {
				var k strings.Builder
				for _, v := range row[:keys] {
					fmt.Fprintf(&k, "%T:%v\x00", v, v)
				}
				m[k.String()] = row
			}
			return m
		}
		before, after := index(old[table]), index(new[table])
		var deleted, upserts [][]any
		for k, row := range before {
			if _, ok := after[k]; !ok {
				deleted = append(deleted, row)
			}
		}
		for k, row := range after {
			if prior, ok := before[k]; !ok || !slices.Equal(prior, row) {
				upserts = append(upserts, row)
			}
		}
		if len(deleted) > 0 && authorityKept[table] {
			return nil, ErrNativeProvisioning
		}
		where := strings.Join(columns[:keys], " = ? AND ") + " = ?"
		for _, row := range deleted {
			if _, err := t.c.ExecContext(t.ctx, "DELETE FROM "+table+" WHERE "+where, row[:keys]...); err != nil {
				return nil, err
			}
			changed[table] = append(changed[table], nil)
		}
		slices.SortFunc(upserts, compareRows)
		conflict := " ON CONFLICT DO NOTHING"
		if len(columns) > keys {
			var set []string
			for _, c := range columns[keys:] {
				set = append(set, c+" = excluded."+c)
			}
			conflict = " ON CONFLICT(" + strings.Join(columns[:keys], ", ") + ") DO UPDATE SET " + strings.Join(set, ", ")
		}
		if len(upserts) == 0 {
			continue
		}
		insert, err := t.c.PrepareContext(t.ctx, "INSERT INTO "+table+"("+strings.Join(columns, ", ")+") VALUES("+strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ")+")"+conflict)
		if err != nil {
			return nil, err
		}
		for _, row := range upserts {
			if _, err := insert.ExecContext(t.ctx, row...); err != nil {
				_ = insert.Close()
				return nil, fmt.Errorf("%s: %w", table, err)
			}
			changed[table] = append(changed[table], row)
		}
		if err := insert.Close(); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// flush writes the Update's puts: the initialization flag, then the ledger
// rows that differ from the stored ones (validated as a whole ledger first),
// then the lifecycle rows that differ. A mailbox or address change bumps
// meta.change, and every changed address's routes_pending row follows it.
func (t *sqliteTx) flush() error {
	if initialized, _ := t.Initialized(); initialized && t.metaValue("initialized") != "1" {
		if _, err := t.c.ExecContext(t.ctx, "UPDATE meta SET value = '1' WHERE key = 'initialized'"); err != nil {
			return err
		}
	}
	if t.accounts != nil {
		stored, err := t.storedLedger()
		if err != nil {
			return err
		}
		base := nativeLedger{Version: 2, AddressGenerations: true, Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}
		if stored != nil {
			base = *stored
		}
		merged := cloneLedger(base)
		maps.Copy(merged.Accounts, t.accounts)
		maps.Copy(merged.Mailboxes, t.mailboxes)
		maps.Copy(merged.Addresses, t.addresses)
		if _, _, err := validateNativeLedger(merged); err != nil {
			return err
		}
		before, after := authorityRows{}, authorityRows{}
		before.ledgerRows(base)
		after.ledgerRows(merged)
		changed, err := t.writeDiff(before, after, ledgerTablesV3)
		if err != nil {
			return err
		}
		if len(changed["mailboxes"])+len(changed["addresses"]) > 0 {
			n, err := t.Change()
			if err != nil {
				return err
			}
			if _, err := t.c.ExecContext(t.ctx, "UPDATE meta SET value = ? WHERE key = 'change'", strconv.FormatInt(n+1, 10)); err != nil {
				return err
			}
		}
		for _, row := range changed["addresses"] {
			query := "INSERT INTO routes_pending(address, generation) VALUES(?, ?) ON CONFLICT(address) DO UPDATE SET generation = excluded.generation"
			args := []any{row[0], row[4]}
			if row[3] == "active" {
				query, args = "DELETE FROM routes_pending WHERE address = ?", args[:1]
			}
			if _, err := t.c.ExecContext(t.ctx, query, args...); err != nil {
				return err
			}
		}
	}
	if t.lifePut != nil {
		stored, err := t.storedLifecycle()
		if err != nil {
			return err
		}
		before, after := authorityRows{}, authorityRows{}
		if err := errors.Join(before.lifecycleRows(stored), after.lifecycleRows(*t.lifePut)); err != nil {
			return err
		}
		if _, err := t.writeDiff(before, after, lifecycleTablesV3); err != nil {
			return err
		}
	}
	return nil
}
