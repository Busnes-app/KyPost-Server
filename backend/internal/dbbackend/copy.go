package dbbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// The table-by-table copy between a SQLite file and its Postgres schema, ported from
// KyIdentity's pgcopy. ExportSQLite writes a schema into a fresh SQLite file with the
// binary's SQLite schema, so a capsule's members are the same on both backends;
// ImportSQLite loads such a file into a schema (migrate-to-postgres, restore). Tables and
// columns are read from both catalogs and must agree exactly.

// Kind is one sort of store database: its SQLite file name, its Postgres DDL, and how to
// create (or bring up to date) a SQLite file with the binary's schema.
type Kind struct {
	Name         string
	File         string
	DDL          string
	CreateSQLite func(path string) error
}

var (
	kindsMu sync.Mutex
	kinds   = map[string]Kind{}
)

// Register makes a kind known to the copies; each store package registers its own.
func Register(k Kind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds[k.Name] = k
}

// KindOf returns the registered kind of a SQLite file name (state.db, mailbox.db...).
func KindOf(file string) (Kind, bool) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	for _, k := range kinds {
		if k.File == file {
			return k, true
		}
	}
	return Kind{}, false
}

func kindNamed(name string) (Kind, bool) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	k, ok := kinds[name]
	return k, ok
}

type copyColumn struct{ name, typ string }

type copyTable struct {
	cols     []copyColumn // by name
	refs     []string     // tables its foreign keys reference
	identity []string     // Postgres identity columns
	rowid    bool         // SQLite: an ordinary rowid table
}

type tables map[string]*copyTable

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (c tables) table(name string) *copyTable {
	if c[name] == nil {
		c[name] = &copyTable{}
	}
	return c[name]
}

func readPairs(ctx context.Context, q querier, query string, each func(table, value string) error, rest ...any) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, value string
		if err := rows.Scan(append([]any{&table, &value}, rest...)...); err != nil {
			return err
		}
		if err := each(table, value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func sqliteTables(ctx context.Context, q querier) (tables, error) {
	c := tables{}
	var typ string
	if err := readPairs(ctx, q, `SELECT m.name, p.name, p.type FROM sqlite_master m, pragma_table_info(m.name) p
 WHERE m.type='table' AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY m.name, p.name`, func(table, col string) error {
		t := c.table(table)
		t.cols = append(t.cols, copyColumn{col, typ})
		return nil
	}, &typ); err != nil {
		return nil, fmt.Errorf("reading the SQLite catalog: %w", err)
	}
	var wr int
	if err := readPairs(ctx, q, `SELECT name, type, wr FROM pragma_table_list WHERE schema='main'`, func(table, kind string) error {
		if t := c[table]; t != nil && kind == "table" && wr == 0 {
			t.rowid = true
		}
		return nil
	}, &wr); err != nil {
		return nil, fmt.Errorf("reading the SQLite catalog: %w", err)
	}
	if err := readPairs(ctx, q, `SELECT DISTINCT m.name, f."table" FROM sqlite_master m, pragma_foreign_key_list(m.name) f
 WHERE m.type='table' AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\'`, func(table, ref string) error {
		c.table(table).refs = append(c.table(table).refs, ref)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading the SQLite foreign keys: %w", err)
	}
	return c, nil
}

func pgTables(ctx context.Context, q querier) (tables, error) {
	c := tables{}
	var typ string
	var identity bool
	if err := readPairs(ctx, q, `SELECT c.relname, a.attname, t.typname, a.attidentity<>'' FROM pg_class c
 JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped JOIN pg_type t ON t.oid=a.atttypid
 WHERE c.relnamespace=current_schema()::regnamespace AND c.relkind='r' ORDER BY 1, 2`, func(table, col string) error {
		t := c.table(table)
		t.cols = append(t.cols, copyColumn{col, typ})
		if identity {
			t.identity = append(t.identity, col)
		}
		return nil
	}, &typ, &identity); err != nil {
		return nil, fmt.Errorf("reading the Postgres catalog: %w", err)
	}
	if err := readPairs(ctx, q, `SELECT DISTINCT c.relname, r.relname FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid
 JOIN pg_class r ON r.oid=k.confrelid WHERE k.contype='f' AND c.relnamespace=current_schema()::regnamespace`, func(table, ref string) error {
		if table != ref {
			c.table(table).refs = append(c.table(table).refs, ref)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading the Postgres foreign keys: %w", err)
	}
	return c, nil
}

func colNames(cols []copyColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}

// errNewer marks a SQLite file with tables or columns this binary does not know.
var errNewer = errors.New("written by a newer release: it has tables or columns this binary does not know; use a release at least as new as the one that wrote it")

// matchTables refuses unless both sides have the same tables with the same columns. The
// Postgres identity column rowid stands for SQLite's implicit rowid, which is then added
// to the SQLite table so the copy carries it. older reports a SQLite side that lacks
// only tables or columns Postgres has (a file from an older release).
func matchTables(lite, pg tables) (older bool, err error) {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(pg)) {
		l := lite[name]
		if l == nil {
			older = true
			continue
		}
		p := pg[name]
		if slices.Contains(p.identity, "rowid") && !slices.Contains(colNames(l.cols), "rowid") && l.rowid {
			l.cols = append(l.cols, copyColumn{"rowid", "INTEGER"})
			slices.SortFunc(l.cols, func(a, b copyColumn) int { return strings.Compare(a.name, b.name) })
		}
		lc, pc := colNames(l.cols), colNames(p.cols)
		for _, c := range lc {
			if !slices.Contains(pc, c) {
				problems = append(problems, "column "+name+"."+c)
			}
		}
		older = older || len(lc) < len(pc)
	}
	for _, name := range slices.Sorted(maps.Keys(lite)) {
		if pg[name] == nil {
			problems = append(problems, "table "+name)
		}
	}
	if len(problems) > 0 {
		return false, fmt.Errorf("%w (%s)", errNewer, strings.Join(problems, ", "))
	}
	return older, nil
}

// fkOrder lists the tables so that every table comes after the tables it references.
func fkOrder(c tables) ([]string, error) {
	var order []string
	done := map[string]bool{}
	names := slices.Sorted(maps.Keys(c))
	for len(order) < len(names) {
		before := len(order)
		for _, name := range names {
			if done[name] {
				continue
			}
			ready := true
			for _, ref := range c[name].refs {
				if c[ref] == nil {
					return nil, fmt.Errorf("table %s references %s, which does not exist", name, ref)
				}
				ready = ready && (done[ref] || ref == name)
			}
			if ready {
				done[name] = true
				order = append(order, name)
			}
		}
		if len(order) == before {
			return nil, errors.New("the foreign keys form a cycle; refusing to guess an insert order")
		}
	}
	return order, nil
}

func toSQLite(v any, decl string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch d := strings.ToUpper(decl); {
	case strings.Contains(d, "INT"):
		if n, ok := v.(int64); ok {
			return n, nil
		}
	case d == "TEXT":
		if s, ok := v.(string); ok {
			return s, nil
		}
		if b, ok := v.([]byte); ok { // a Postgres BYTEA kept for text SQLite holds with NUL
			return string(b), nil
		}
	case d == "BLOB":
		if b, ok := v.([]byte); ok {
			return append([]byte{}, b...), nil // never nil: that would be NULL
		}
	default:
		return nil, fmt.Errorf("no mapping for SQLite type %q", decl)
	}
	return nil, fmt.Errorf("a %T value does not convert to SQLite %s", v, decl)
}

func toPostgres(v any, typ string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case "int8":
		if n, ok := v.(int64); ok {
			return n, nil
		}
	case "text", "citext":
		s, ok := v.(string)
		if b, isBytes := v.([]byte); isBytes {
			s, ok = string(b), true
		}
		if ok && utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
			return s, nil
		}
	case "bytea":
		if b, ok := v.([]byte); ok {
			return append([]byte{}, b...), nil
		}
		if s, ok := v.(string); ok {
			return []byte(s), nil
		}
	default:
		return nil, fmt.Errorf("no mapping for Postgres type %s", typ)
	}
	return nil, fmt.Errorf("a %T value does not convert to Postgres %s", v, typ)
}

type execPreparer interface {
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// copyRows copies every row of one table, in rowid order where it has one. An error
// names the table and column of a value that does not convert, never the value.
func copyRows(ctx context.Context, src querier, dst execPreparer, name string, cols []copyColumn, convert func(any, string) (any, error)) (int64, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = QuoteIdent(c.name)
	}
	list := strings.Join(quoted, ", ")
	sel := "SELECT " + list + " FROM " + QuoteIdent(name)
	if slices.Contains(colNames(cols), "rowid") {
		sel += ` ORDER BY "rowid"`
	}
	rows, err := src.QueryContext(ctx, sel)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	defer rows.Close()
	ins, err := dst.PrepareContext(ctx, "INSERT INTO "+QuoteIdent(name)+" ("+list+") VALUES ("+strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")+")")
	if err != nil {
		return 0, fmt.Errorf("writing %s: %w", name, err)
	}
	defer ins.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, fmt.Errorf("reading %s: %w", name, err)
		}
		if copyHook != nil && copyHook(name, vals) {
			continue
		}
		for i, c := range cols {
			if vals[i], err = convert(vals[i], c.typ); err != nil {
				return n, fmt.Errorf("table %s column %s: %w", name, c.name, err)
			}
		}
		if _, err := ins.ExecContext(ctx, vals...); err != nil {
			return n, fmt.Errorf("writing %s: %w", name, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("reading %s: %w", name, err)
	}
	return n, nil
}

// sqliteURI is a file: URI for path with query.
func sqliteURI(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

// ExportSQLite writes the database the store at path keeps on Postgres into dest, a new
// SQLite file with the binary's SQLite schema, from one REPEATABLE READ READ ONLY
// transaction: one point in time, no writer lock. dest is created 0600 and removed on
// any error.
func ExportSQLite(ctx context.Context, path, dest string) (err error) {
	cat, err := root(ctx)
	if err != nil {
		return err
	}
	schema, rel, err := SchemaName(path)
	if err != nil {
		return err
	}
	var kindName string
	if err := cat.QueryRowContext(ctx, `SELECT kind FROM kypost_catalog.stores WHERE path=? AND schema=?`, rel, schema).Scan(&kindName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return err
	}
	return exportSchema(ctx, schema, kindName, dest)
}

func exportSchema(ctx context.Context, schema, kindName, dest string) (err error) {
	kind, ok := kindNamed(kindName)
	if !ok {
		return fmt.Errorf("no SQLite form for %s stores", kindName)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("creating the snapshot: %s exists", filepath.Base(dest))
	}
	// The kind's SQLite opener wants its own file name in an owner-only directory.
	work, err := os.MkdirTemp(filepath.Dir(dest), ".export-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	defer func() {
		if err != nil {
			err = fmt.Errorf("database snapshot failed: %w", err)
		}
	}()
	file := filepath.Join(work, kind.File)
	if err := kind.CreateSQLite(file); err != nil {
		return err
	}
	if err := exportInto(ctx, schema, kindName, file); err != nil {
		return err
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return err
	}
	return os.Rename(file, dest)
}

func exportInto(ctx context.Context, schema, kindName, dest string) error {
	lite, err := sql.Open("sqlite", sqliteURI(dest, "_pragma=foreign_keys(0)&_pragma=busy_timeout(5000)"))
	if err != nil {
		return err
	}
	defer lite.Close()
	lite.SetMaxOpenConns(1) // the pragmas hold for one connection
	pool, err := Pool(schema, true)
	if err != nil {
		return err
	}
	defer pool.Close()
	src, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = src.Rollback() }()
	// A large table keeps the snapshot open while SQLite writes; not the pool's 30 s limit.
	if _, err := src.ExecContext(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return err
	}
	pg, err := pgTables(ctx, src)
	if err != nil {
		return err
	}
	lt, err := sqliteTables(ctx, lite)
	if err != nil {
		return err
	}
	if older, err := matchTables(lt, pg); err != nil || older {
		return fmt.Errorf("the SQLite and Postgres %s schemas differ: %v", kindName, err)
	}
	order, err := fkOrder(lt)
	if err != nil {
		return err
	}
	type trigger struct{ name, sql string }
	var triggers []trigger
	var body string
	if err := readPairs(ctx, lite, `SELECT 'trigger', name, sql FROM sqlite_master WHERE type='trigger' ORDER BY name`, func(_, name string) error {
		triggers = append(triggers, trigger{name, body})
		return nil
	}, &body); err != nil {
		return err
	}
	tx, err := lite.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range triggers {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+QuoteIdent(t.name)); err != nil {
			return err
		}
	}
	for _, name := range order {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+QuoteIdent(name)); err != nil {
			return err
		}
		if _, err := copyRows(ctx, src, tx, name, lt[name].cols, toSQLite); err != nil {
			return err
		}
	}
	for _, t := range triggers {
		if _, err := tx.ExecContext(ctx, t.sql); err != nil {
			return fmt.Errorf("recreating trigger %s: %w", t.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := sqliteChecks(ctx, lite); err != nil {
		return err
	}
	// Rollback journal, as VACUUM INTO writes: the file alone is the whole database.
	_, err = lite.ExecContext(ctx, `PRAGMA journal_mode=DELETE`)
	return err
}

func sqliteChecks(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	if rows.Next() {
		var table string
		_ = rows.Scan(&table)
		rows.Close()
		return fmt.Errorf("foreign key check failed in table %s", table)
	}
	rows.Close()
	var ok string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil {
		return err
	}
	if ok != "ok" {
		return fmt.Errorf("integrity check failed: %s", ok)
	}
	return nil
}

// OpenSQLiteSource opens a SQLite file to copy from without writing it or anything beside
// it: a -wal beside it is refused (stop the server that uses it first), and a file from an
// older release is brought up to date on a private copy in scratch, which done removes.
func OpenSQLiteSource(ctx context.Context, path, kindName, scratch string) (db *sql.DB, done func(), err error) {
	kind, ok := kindNamed(kindName)
	if !ok {
		return nil, nil, fmt.Errorf("unknown store kind %s", kindName)
	}
	// An empty log (a reader still open) holds nothing the immutable open would miss.
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() > 0 {
		return nil, nil, fmt.Errorf("%s has a write-ahead log beside it: stop every KyPost process that uses it (a clean stop folds the log into the file) and try again", filepath.Base(path))
	}
	open := func(p string) (*sql.DB, error) {
		db, err := sql.Open("sqlite", sqliteURI(p, "mode=ro&immutable=1"))
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		return db, nil
	}
	if db, err = open(path); err != nil {
		return nil, nil, err
	}
	lt, err := sqliteTables(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	pgSide, err := ddlTables(ctx, kind)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	older, err := matchTables(lt, pgSide)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if !older {
		return db, func() { _ = db.Close() }, nil
	}
	_ = db.Close()
	dir, err := os.MkdirTemp(scratch, "upgrade-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyPath := filepath.Join(dir, kind.File)
	if err = copyFile(path, copyPath); err == nil {
		err = kind.CreateSQLite(copyPath)
	}
	if err == nil {
		db, err = open(copyPath)
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("bringing a copy of the older file %s up to date: %w", filepath.Base(path), err)
	}
	return db, func() { _ = db.Close(); cleanup() }, nil
}

// ddlTables is the Postgres catalog the kind's DDL creates, read from a scratch schema
// that is rolled back.
func ddlTables(ctx context.Context, kind Kind) (tables, error) {
	probedMu.Lock()
	defer probedMu.Unlock()
	if t, ok := probed[kind.DDL]; ok {
		return t.clone(), nil
	}
	t, err := probeTables(ctx, kind)
	if err != nil {
		return nil, err
	}
	probed[kind.DDL] = t
	return t.clone(), nil
}

var (
	probedMu sync.Mutex
	probed   = map[string]tables{}
)

// clone copies the tables, which matchTables edits.
func (c tables) clone() tables {
	out := tables{}
	for name, t := range c {
		cp := *t
		cp.cols = slices.Clone(t.cols)
		out[name] = &cp
	}
	return out
}

func probeTables(ctx context.Context, kind Kind) (tables, error) {
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := cat.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA kypost_probe; SET LOCAL search_path TO kypost_probe, public`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, kind.DDL); err != nil {
		return nil, err
	}
	return pgTables(ctx, tx)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ImportSQLite copies the SQLite database src into the schema for rel, inside tx (a
// Begin transaction): it creates the schema with its catalog row, refuses one holding
// rows, copies every table in foreign key order with user triggers disabled and moves
// each identity past its largest value. It returns the rows copied per table.
// rel is the catalog path ("state/users/<id>/state.db"), relative to the data roots.
func ImportSQLite(ctx context.Context, tx *sql.Tx, rel, kindName string, src *sql.DB) (map[string]int64, error) {
	kind, ok := kindNamed(kindName)
	if !ok {
		return nil, fmt.Errorf("unknown store kind %s", kindName)
	}
	schema := SchemaForRel(rel)
	if err := CreateSchemaTx(ctx, tx, schema, rel, kind.Name, kind.DDL); err != nil {
		return nil, err
	}
	pg, err := pgTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	lt, err := sqliteTables(ctx, src)
	if err != nil {
		return nil, err
	}
	if older, err := matchTables(lt, pg); err != nil || older {
		return nil, fmt.Errorf("%s: the SQLite and Postgres schemas differ: %v", rel, err)
	}
	order, err := fkOrder(pg)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, name := range order {
		var has bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+QuoteIdent(name)+`)`).Scan(&has); err != nil {
			return nil, err
		}
		if has {
			held = append(held, name)
		}
	}
	if len(held) > 0 {
		return nil, fmt.Errorf("%w: %s holds rows in %s", ErrNotEmpty, rel, strings.Join(held, ", "))
	}
	for _, name := range order {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+QuoteIdent(name)+` DISABLE TRIGGER USER`); err != nil {
			return nil, err
		}
	}
	counts := map[string]int64{}
	for _, name := range order {
		n, err := copyRows(ctx, src, tx, name, pg[name].cols, toPostgres)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		counts[name] = n
	}
	// Deferred foreign keys are checked now: ALTER TABLE refuses pending checks.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	// Every table's content, not just its count, must match the source before the commit.
	for _, name := range order {
		if err := verifyTable(ctx, src, tx, name, pg[name].cols); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
	}
	// ALTER ... RESTART is transactional, unlike setval.
	for _, name := range order {
		for _, col := range pg[name].identity {
			var next int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+QuoteIdent(col)+`), 0) + 1 FROM `+QuoteIdent(name)).Scan(&next); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s RESTART WITH %d`, QuoteIdent(name), QuoteIdent(col), next)); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+QuoteIdent(name)+` ENABLE TRIGGER USER`); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// ErrNotEmpty is a migration or restore target that already holds rows.
var ErrNotEmpty = errors.New("the Postgres database is not empty")

// Store is one catalogued database.
type Store struct{ Path, Schema, Kind string }

// Stores lists the catalog, in path order.
func Stores(ctx context.Context) ([]Store, error) {
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := cat.QueryContext(ctx, `SELECT path, schema, kind FROM kypost_catalog.stores ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Store
	for rows.Next() {
		var s Store
		if err := rows.Scan(&s.Path, &s.Schema, &s.Kind); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RowCounts counts every table of every catalogued schema in one snapshot, keyed by
// catalog path then table: the check that a migration or restore committed what it
// copied, and that a target is empty.
func RowCounts(ctx context.Context) (map[string]map[string]int64, error) {
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := cat.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT path, schema FROM kypost_catalog.stores ORDER BY path`)
	if err != nil {
		return nil, err
	}
	type entry struct{ path, schema string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.path, &e.schema); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+QuoteIdent(e.schema)+`, public`); err != nil {
			return nil, err
		}
		pg, err := pgTables(ctx, tx)
		if err != nil {
			return nil, err
		}
		out[e.path] = map[string]int64{}
		for name := range pg {
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+QuoteIdent(name)).Scan(&n); err != nil {
				return nil, err
			}
			out[e.path][name] = n
		}
	}
	return out, nil
}

// fingerprintSQL describes the current schema without its name: columns with type, null
// rule and default, constraints, indexes, triggers and function bodies.
const fingerprintSQL = `SELECT replace(line, current_schema() || '.', '') FROM (
SELECT 'col ' || c.relname || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || ' ' || a.attnotnull::text || ' ' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '') || ' ' || a.attidentity::text AS line
 FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'r'
UNION ALL SELECT 'con ' || k.conrelid::regclass::text || ' ' || k.conname || ' ' || pg_get_constraintdef(k.oid)
 FROM pg_constraint k WHERE k.connamespace = current_schema()::regnamespace
UNION ALL SELECT 'idx ' || pg_get_indexdef(i.indexrelid)
 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relnamespace = current_schema()::regnamespace
UNION ALL SELECT 'trg ' || pg_get_triggerdef(t.oid)
 FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE c.relnamespace = current_schema()::regnamespace AND NOT t.tgisinternal
UNION ALL SELECT 'fn ' || p.proname || ' ' || md5(p.prosrc)
 FROM pg_proc p WHERE p.pronamespace = current_schema()::regnamespace) f
ORDER BY 1`

// Fingerprint describes the schema q's search_path is on, for an exact-schema check.
func Fingerprint(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, fingerprintSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DDLFingerprint is the Fingerprint of a schema built from ddl alone, in a scratch schema
// that is rolled back.
func DDLFingerprint(ctx context.Context, ddl string) ([]string, error) {
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := cat.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA kypost_probe; SET LOCAL search_path TO kypost_probe, public`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return nil, err
	}
	return Fingerprint(ctx, tx)
}

// Undo removes what an import committed, in one transaction: the schemas at rels that
// were not catalogued before (drop) and the rows of those that were (they were empty).
func Undo(ctx context.Context, rels []string, before map[string]map[string]int64) error {
	tx, err := Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, rel := range rels {
		schema := SchemaForRel(rel)
		if _, existed := before[rel]; !existed {
			if err := dropTx(ctx, tx, schema, rel); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+QuoteIdent(schema)+`, public`); err != nil {
			return err
		}
		pg, err := pgTables(ctx, tx)
		if err != nil {
			return err
		}
		var quoted []string
		for name := range pg {
			quoted = append(quoted, QuoteIdent(name))
		}
		if len(quoted) > 0 {
			if _, err := tx.ExecContext(ctx, `TRUNCATE `+strings.Join(quoted, ", ")); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// StoresUnder lists the live paths of the published (not pending) databases of a kind
// under any of dirs.
func StoresUnder(ctx context.Context, kind string, dirs ...string) ([]string, error) {
	stores, err := Stores(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, st := range stores {
		if st.Kind != kind {
			continue
		}
		live, err := filepath.Abs(st.LivePath())
		if err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			if abs, err := filepath.Abs(dir); err == nil && strings.HasPrefix(live, abs+string(filepath.Separator)) {
				out = append(out, live)
				break
			}
		}
	}
	return out, nil
}

// copyHook lets a test drop (true) or alter a row on its way into the copy, to prove the
// verification catches it. nil outside tests.
var copyHook func(table string, row []any) (drop bool)

// ErrDigestMismatch is a copied table whose content differs from its source.
var ErrDigestMismatch = errors.New("the copied rows differ from the source")

// verifyTable compares one table on both sides by row count and an order-independent digest
// of its rows: each row's values normalized by the Postgres column type (integers as
// decimal, text as its bytes, BLOB byte-exact, NULL distinct from empty), hashed, the
// hashes sorted and hashed together. A value lost, changed or added fails it.
func verifyTable(ctx context.Context, src, dst querier, name string, cols []copyColumn) error {
	want, wantN, err := tableDigest(ctx, src, name, cols)
	if err != nil {
		return fmt.Errorf("digesting source %s: %w", name, err)
	}
	got, gotN, err := tableDigest(ctx, dst, name, cols)
	if err != nil {
		return fmt.Errorf("digesting copied %s: %w", name, err)
	}
	if wantN != gotN || want != got {
		return fmt.Errorf("%w: table %s (%d rows in the source, %d copied)", ErrDigestMismatch, name, wantN, gotN)
	}
	return nil
}

func tableDigest(ctx context.Context, q querier, name string, cols []copyColumn) ([32]byte, int64, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = QuoteIdent(c.name)
	}
	rows, err := q.QueryContext(ctx, "SELECT "+strings.Join(quoted, ", ")+" FROM "+QuoteIdent(name))
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var hashes [][32]byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return [32]byte{}, 0, err
		}
		h := sha256.New()
		for i, c := range cols {
			v, err := toPostgres(vals[i], c.typ)
			if err != nil {
				return [32]byte{}, 0, fmt.Errorf("column %s: %w", c.name, err)
			}
			switch v := v.(type) {
			case nil:
				h.Write([]byte{'n'})
			case int64:
				fmt.Fprintf(h, "i%d;", v)
			case string:
				fmt.Fprintf(h, "t%d:%s", len(v), v)
			case []byte:
				fmt.Fprintf(h, "b%d:", len(v))
				h.Write(v)
			}
		}
		hashes = append(hashes, [32]byte(h.Sum(nil)))
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, 0, err
	}
	slices.SortFunc(hashes, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	all := sha256.New()
	for _, h := range hashes {
		all.Write(h[:])
	}
	return [32]byte(all.Sum(nil)), int64(len(hashes)), nil
}
