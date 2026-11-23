// Package dbbackend chooses where the stores keep their databases: a SQLite file each (the
// default), or one schema each in a single Postgres database when KYPOST_DATABASE_URL or
// KYPOST_DATABASE_URL_FILE is set. A store still names its database by the SQLite path it
// would use; on Postgres that path picks the schema (SchemaName) and the catalog table
// kypost_catalog.stores records it, so backup, migration and restore list the schemas
// without reversing names.
package dbbackend

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

const (
	EnvURL     = "KYPOST_DATABASE_URL"
	EnvURLFile = "KYPOST_DATABASE_URL_FILE"
	// EnvTestURL puts a test binary's stores on Postgres; it is ignored outside tests.
	EnvTestURL = "KYPOST_TEST_DATABASE_URL"
)

var (
	ErrBoth    = errors.New("set KYPOST_DATABASE_URL or KYPOST_DATABASE_URL_FILE, not both")
	ErrURLFile = errors.New("KYPOST_DATABASE_URL_FILE cannot be read or is empty")
	// ErrNotFound is a missing database on Postgres; it is an os.ErrNotExist, as a
	// missing file is.
	ErrNotFound = fmt.Errorf("database not found in the Postgres catalog: %w", os.ErrNotExist)
)

type selection struct {
	url string // "" is SQLite
	err error  // a Postgres configuration that cannot be used; never falls back to SQLite
}

var (
	mu       sync.Mutex
	selected *selection
	catalog  *sql.DB // the pool with no search_path: catalog reads and writes
	ready    bool    // catalog and citext exist
)

func current() selection {
	mu.Lock()
	defer mu.Unlock()
	if selected == nil {
		s := load()
		selected = &s
	}
	return *selected
}

func load() selection {
	raw, file := os.Getenv(EnvURL), os.Getenv(EnvURLFile)
	switch {
	case raw != "" && file != "":
		return selection{err: ErrBoth}
	case file != "":
		b, err := os.ReadFile(file)
		raw = strings.TrimSpace(string(b))
		if err != nil || raw == "" {
			return selection{err: ErrURLFile}
		}
	case raw == "" && testHook():
		raw = os.Getenv(EnvTestURL)
	}
	if raw == "" {
		return selection{}
	}
	// A URL, not a keyword DSN: the stores set search_path in its query.
	if u, err := url.Parse(raw); err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return selection{url: raw, err: pgdialect.ErrBadURL}
	}
	if _, err := pgdialect.Config(raw); err != nil {
		return selection{url: raw, err: pgdialect.ErrBadURL}
	}
	return selection{url: raw}
}

// Postgres reports whether the stores run on Postgres. A configuration error still
// reports true: every open then fails with it instead of falling back to SQLite.
func Postgres() bool {
	s := current()
	return s.url != "" || s.err != nil
}

// Name is the backend for health and status: "postgres" or "sqlite".
func Name() string {
	if Postgres() {
		return "postgres"
	}
	return "sqlite"
}

// Describe names the database server for logs: host and database name only.
func Describe() string { return pgdialect.Describe(current().url) }

// Check validates the configuration and the server before anything is written: one
// variable, a URL that parses, Postgres 16 or newer, UTF8 with the C locale. It then
// creates citext and the catalog. Errors never carry the URL. A nil error on SQLite.
func Check(ctx context.Context) error {
	if !Postgres() {
		return nil
	}
	_, err := root(ctx)
	return err
}

func root(ctx context.Context) (*sql.DB, error) {
	s := current()
	if s.err != nil {
		return nil, s.err
	}
	mu.Lock()
	defer mu.Unlock()
	if catalog == nil {
		c, err := pgdialect.NewConnector(s.url, false)
		if err != nil {
			return nil, err
		}
		catalog = sql.OpenDB(c)
		catalog.SetMaxOpenConns(4)
		catalog.SetMaxIdleConns(1)
		catalog.SetConnMaxIdleTime(time.Minute)
	}
	if ready {
		return catalog, nil
	}
	where := pgdialect.Describe(s.url)
	if err := catalog.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connecting to Postgres at %s: %w", where, scrub(err, s.url))
	}
	if err := checkServer(ctx, catalog); err != nil {
		return nil, fmt.Errorf("Postgres at %s: %w", where, scrub(err, s.url))
	}
	if _, err := catalog.ExecContext(ctx, catalogDDL); err != nil {
		return nil, fmt.Errorf("creating the store catalog on %s: %w", where, scrub(err, s.url))
	}
	if testHook() {
		if err := dropStaleTestSchemas(ctx, catalog); err != nil {
			return nil, err
		}
	}
	ready = true
	slog.Info("database: Postgres", "action", "connect", "target", where)
	return catalog, nil
}

// scrub removes the URL and its password from a driver error, which may quote a DSN.
func scrub(err error, rawURL string) error {
	msg := err.Error()
	if u, e := url.Parse(rawURL); e == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			msg = strings.ReplaceAll(msg, p, "***")
		}
	}
	if strings.Contains(msg, rawURL) {
		msg = strings.ReplaceAll(msg, rawURL, "(database URL)")
	}
	return errors.New(msg)
}

// checkServer refuses a server or database whose text rules differ from SQLite's. NOCASE
// and lower() fold ASCII only and compare bytes, which on Postgres needs the C locale and
// citext; labels, addresses and routing rest on those comparisons. The version and
// encoding are read before anything is created.
func checkServer(ctx context.Context, db *sql.DB) error {
	var recent, utf8, lowerASCII, bytewise bool
	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int>=160000, current_setting('server_encoding')='UTF8', lower('É')='É', 'B'<'a'`).Scan(&recent, &utf8, &lowerASCII, &bytewise); err != nil {
		return fmt.Errorf("checking the server: %w", err)
	}
	if !recent {
		return errors.New("Postgres 16 or newer is required")
	}
	if !utf8 || !lowerASCII || !bytewise {
		return errLocale
	}
	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS citext SCHEMA public`); err != nil {
		return fmt.Errorf("creating the citext extension: %w", err)
	}
	var keepsAccents, foldsASCII bool
	if err := db.QueryRowContext(ctx, `SELECT 'É'::public.citext<>'é'::public.citext, 'A'::public.citext='a'::public.citext`).Scan(&keepsAccents, &foldsASCII); err != nil {
		return fmt.Errorf("checking citext: %w", err)
	}
	if !keepsAccents || !foldsASCII {
		return errLocale
	}
	return nil
}

var errLocale = errors.New("the Postgres database must use encoding UTF8 with the C locale (LC_COLLATE and LC_CTYPE C), so names compare exactly as on SQLite; create it with CREATE DATABASE kypost ENCODING 'UTF8' LOCALE 'C' TEMPLATE template0")

const catalogDDL = `CREATE SCHEMA IF NOT EXISTS kypost_catalog;
CREATE TABLE IF NOT EXISTS kypost_catalog.stores (
 path TEXT PRIMARY KEY, schema TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, ddl TEXT NOT NULL)`

// SchemaName maps a database path to its schema and catalog path. Under STATE_DIR or
// CONFIG_DIR the catalog path is "state/<rel>" or "config/<rel>"; the install-wide
// files get readable names and every other file (one per account or mailbox) a fixed
// prefix and a hash, so a name is at most 41 bytes whatever the IDs are. Names hold only
// [a-z0-9_] and are still quoted wherever they reach SQL. A test binary maps any other
// path to a schema of its own.
func SchemaName(path string) (schema, rel string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	for _, r := range []struct{ dir, prefix string }{{config.StateDir(), "state"}, {config.ConfigDir(), "config"}} {
		if testNames() {
			break
		}
		dir, e := filepath.Abs(r.dir)
		if e != nil {
			continue
		}
		if p, e := filepath.Rel(dir, abs); e == nil && p != "." && p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			rel = r.prefix + "/" + filepath.ToSlash(p)
			return SchemaForRel(rel), rel, nil
		}
	}
	if testHook() {
		sum := sha256.Sum256([]byte(abs))
		return "kt_" + hex.EncodeToString(sum[:12]), abs, nil
	}
	return "", "", errors.New("a database path is outside STATE_DIR and CONFIG_DIR")
}

// SchemaForRel is the schema of a catalog path ("state/users/<id>/state.db").
func SchemaForRel(rel string) string {
	if name, ok := rootSchemas[rel]; ok {
		return name
	}
	sum := sha256.Sum256([]byte(rel))
	return "kypost_f_" + hex.EncodeToString(sum[:16])
}

var rootSchemas = map[string]string{
	"state/state.db":                "kypost_state",
	"state/receiving/ingress.db":    "kypost_ingress",
	"state/receiving/cloudflare.db": "kypost_cloudflare",
	"config/native-authority.db":    "kypost_native_authority",
	"state/volumes.db":              "kypost_volumes", // table-mode volume documents
	"state/tenancy.db":              "kypost_tenancy", // multi-tenant installs only
}

// Options say how Open treats a store's schema.
type Options struct {
	Kind     string // the store kind recorded in the catalog; a mismatch is refused ("" opens any)
	DDL      string // Postgres DDL, run when the schema is created or the DDL changed
	Create   bool   // create a missing schema; otherwise a missing one is ErrNotFound
	ReadOnly bool   // a read-only pool: REPEATABLE READ snapshots, no writer lock
}

// Open returns a pool on the schema for path. Each schema has its own writer lock, which
// keeps SQLite's one writer per file; reads take no lock.
//
// ponytail: one pool per open store, at most 4 connections and 1 idle for 15 s. The
// ceiling is max_connections on busy installs with many open stores; the upgrade path
// is one shared pgxpool that sets search_path per checkout.
func Open(path string, o Options) (*sql.DB, error) {
	ctx := context.Background()
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	schema, rel, err := SchemaName(path)
	if err != nil {
		return nil, err
	}
	digest := ddlDigest(o.DDL)
	var kind, stored, held string
	err = cat.QueryRowContext(ctx, `SELECT kind, ddl, schema FROM kypost_catalog.stores WHERE path=?`, rel).Scan(&kind, &stored, &held)
	switch {
	case err == nil && held != schema:
		return nil, fmt.Errorf("the catalog maps %s to schema %s, not %s", rel, held, schema)
	case errors.Is(err, sql.ErrNoRows) && !o.Create:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
	case errors.Is(err, sql.ErrNoRows), err == nil && kind == o.Kind && o.DDL != "" && stored != digest && !o.ReadOnly:
		if err = ensureSchema(ctx, cat, schema, rel, o); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, scrub(err, current().url)
	case strings.HasPrefix(kind, pendingPrefix):
		return nil, fmt.Errorf("%s is still being prepared", rel)
	case o.Kind != "" && kind != o.Kind:
		return nil, fmt.Errorf("%s is a %s store, not %s", rel, kind, o.Kind)
	}
	return Pool(schema, o.ReadOnly)
}

// Pool opens a pool on an existing schema without consulting the catalog.
func Pool(schema string, readOnly bool) (*sql.DB, error) {
	s := current()
	if s.err != nil {
		return nil, s.err
	}
	c, err := pgdialect.NewConnector(WithSearchPath(s.url, schema), readOnly)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(15 * time.Second)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

// WithSearchPath sets the URL's search_path to schema, then public (citext).
func WithSearchPath(rawURL, schema string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL // NewConnector reports it
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String()
}

func ddlDigest(ddl string) string {
	sum := sha256.Sum256([]byte(ddl))
	return hex.EncodeToString(sum[:])
}

// QuoteIdent quotes an identifier for SQL.
func QuoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// ensureSchema creates the schema, runs the DDL and records the catalog row in one
// transaction under the catalog's writer lock, so two processes opening a new store at
// once create it once.
func ensureSchema(ctx context.Context, cat *sql.DB, schema, rel string, o Options) error {
	tx, err := cat.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := CreateSchemaTx(ctx, tx, schema, rel, o.Kind, o.DDL); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateSchemaTx is ensureSchema inside the caller's transaction (migration, restore,
// mailbox preparation). It leaves search_path on the schema for the rest of tx.
func CreateSchemaTx(ctx context.Context, tx *sql.Tx, schema, rel, kind, ddl string) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT kind FROM kypost_catalog.stores WHERE path=?`, rel).Scan(&existing)
	if err == nil && existing != kind {
		return fmt.Errorf("%s is a %s store, not %s", rel, existing, kind)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+QuoteIdent(schema)+`; SET LOCAL search_path TO `+QuoteIdent(schema)+`, public`); err != nil {
		return err
	}
	if ddl != "" {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating the %s schema: %w", kind, err)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kypost_catalog.stores(path, schema, kind, ddl) VALUES(?,?,?,?)
 ON CONFLICT(path) DO UPDATE SET ddl=excluded.ddl`, rel, schema, kind, ddlDigest(ddl))
	return err
}

// Exists reports whether the catalog holds the database for path: the Postgres answer to
// "does the file exist".
func Exists(path string) (bool, error) {
	ctx := context.Background()
	cat, err := root(ctx)
	if err != nil {
		return false, err
	}
	schema, rel, err := SchemaName(path)
	if err != nil {
		return false, err
	}
	var n int
	err = cat.QueryRowContext(ctx, `SELECT count(*) FROM kypost_catalog.stores WHERE path=? AND schema=?`, rel, schema).Scan(&n)
	return n > 0, err
}

// Remove drops the schemas of every database at or under path and their catalog rows, in
// one transaction: the Postgres side of deleting an account's directory.
func Remove(path string) error {
	ctx := context.Background()
	cat, err := root(ctx)
	if err != nil {
		return err
	}
	_, rel, err := SchemaName(filepath.Join(path, "x.db"))
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(rel, "x.db")
	tx, err := cat.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT schema FROM kypost_catalog.stores WHERE left(path, length(?))=?`, prefix, prefix)
	if err != nil {
		return err
	}
	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		schemas = append(schemas, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range schemas {
		if _, err := tx.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+QuoteIdent(s)+` CASCADE`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kypost_catalog.stores WHERE left(path, length(?))=?`, prefix, prefix); err != nil {
		return err
	}
	return tx.Commit()
}

// Begin starts a transaction on the catalog pool, under its writer lock: for work that
// spans schemas (publishing a prepared mailbox, migration, restore).
func Begin(ctx context.Context) (*sql.Tx, error) {
	cat, err := root(ctx)
	if err != nil {
		return nil, err
	}
	return cat.BeginTx(ctx, nil)
}

// SchemaBytes is the size of the tables, indexes and TOAST of the schema db's pool is on:
// DiskUsageBytes on Postgres. Zero when unreadable, as a missing file is on SQLite.
func SchemaBytes(db *sql.DB) int64 {
	var n int64
	_ = db.QueryRow(`SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c
 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'r'`).Scan(&n)
	return n
}

// SQL picks a statement's text for the backend: the SQLite text unchanged, or a Postgres
// one where the translator cannot cover the difference (types it cannot infer, SQLite
// functions or catalogs).
func SQL(sqlite, postgres string) string {
	if Postgres() {
		return postgres
	}
	return sqlite
}

// Preparing a directory of new databases (mailbox preparation) on Postgres: Stage moves
// the databases at the keys' paths to the values' paths in one transaction, marked
// pending, so no open finds them; the caller then publishes the directory and Settles
// them, or Unstages them if publication failed. A pending database left at a destination
// by an earlier attempt is dropped first; a settled one is refused, never replaced.
// beforeCommit runs last inside Stage's transaction.
func Stage(ctx context.Context, moves map[string]string, beforeCommit func() error) error {
	tx, err := Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for from, to := range moves {
		oldSchema, oldRel, err := SchemaName(from)
		if err != nil {
			return err
		}
		newSchema, newRel, err := SchemaName(to)
		if err != nil {
			return err
		}
		var kind string
		switch err := tx.QueryRowContext(ctx, `SELECT kind FROM kypost_catalog.stores WHERE path=?`, newRel).Scan(&kind); {
		case err == nil && !strings.HasPrefix(kind, pendingPrefix):
			return fmt.Errorf("%s already holds a database", newRel)
		case err == nil:
			if err := dropTx(ctx, tx, newSchema, newRel); err != nil {
				return err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE kypost_catalog.stores SET path=?, schema=?, kind=?||kind WHERE path=? AND schema=?`, newRel, newSchema, pendingPrefix, oldRel, oldSchema)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return errors.Join(fmt.Errorf("%w: %s", ErrNotFound, oldRel), err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER SCHEMA `+QuoteIdent(oldSchema)+` RENAME TO `+QuoteIdent(newSchema)); err != nil {
			return err
		}
		if testHook() {
			if _, err := tx.ExecContext(ctx, `COMMENT ON SCHEMA `+QuoteIdent(newSchema)+` IS 'path:`+strings.ReplaceAll(newRel, "'", "''")+`'`); err != nil {
				return err
			}
		}
	}
	if err := beforeCommit(); err != nil {
		return err
	}
	return tx.Commit()
}

const pendingPrefix = "pending:"

// Settle makes the pending databases at paths openable. Settled ones are left alone.
func Settle(ctx context.Context, paths ...string) error {
	tx, err := Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range paths {
		_, rel, err := SchemaName(p)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kypost_catalog.stores SET kind=substr(kind, ?) WHERE path=? AND left(kind, ?)=?`, len(pendingPrefix)+1, rel, len(pendingPrefix), pendingPrefix); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Unstage drops the pending databases at paths. Settled ones are left alone.
func Unstage(ctx context.Context, paths ...string) error {
	tx, err := Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range paths {
		schema, rel, err := SchemaName(p)
		if err != nil {
			return err
		}
		var kind string
		err = tx.QueryRowContext(ctx, `SELECT kind FROM kypost_catalog.stores WHERE path=? AND schema=?`, rel, schema).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !strings.HasPrefix(kind, pendingPrefix) {
			continue
		}
		if err != nil {
			return err
		}
		if err := dropTx(ctx, tx, schema, rel); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func dropTx(ctx context.Context, tx *sql.Tx, schema, rel string) error {
	if _, err := tx.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+QuoteIdent(schema)+` CASCADE`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM kypost_catalog.stores WHERE path=?`, rel)
	return err
}

// OpenFile opens an existing database for a helper that opens the file directly rather
// than through its store: a Postgres schema where Held says so (read-only when query has
// mode=ro), else the SQLite file at path with the URI query. A missing database is an
// os.ErrNotExist.
func OpenFile(path, query string) (*sql.DB, error) {
	if pg, err := Held(path); err != nil {
		return nil, err
	} else if !pg {
		return OpenSQLiteFile(path, query)
	}
	ro := false
	if q, err := url.ParseQuery(query); err == nil {
		ro = q.Get("mode") == "ro"
	}
	return Open(path, Options{ReadOnly: ro})
}

// OpenSQLiteFile opens the existing SQLite file at path with the URI query, on either
// backend: restore staging, capsule members and the copies' SQLite side.
func OpenSQLiteFile(path, query string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: query}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// LivePath is the database path a catalog path names: under STATE_DIR or CONFIG_DIR, or
// itself (a test schema's absolute path).
func (s Store) LivePath() string {
	if rest, ok := strings.CutPrefix(s.Path, "state/"); ok {
		return filepath.Join(config.StateDir(), filepath.FromSlash(rest))
	}
	if rest, ok := strings.CutPrefix(s.Path, "config/"); ok {
		return filepath.Join(config.ConfigDir(), filepath.FromSlash(rest))
	}
	return s.Path
}

// Pending reports a database still being prepared (Stage), not yet published.
func (s Store) Pending() bool { return strings.HasPrefix(s.Kind, pendingPrefix) }

// KindBytes is the size of every catalogued schema of the given kinds.
func KindBytes(ctx context.Context, kinds ...string) (int64, error) {
	cat, err := root(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = cat.QueryRowContext(ctx, `SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c
 JOIN pg_namespace ns ON ns.oid = c.relnamespace JOIN kypost_catalog.stores s ON s.schema = ns.nspname
 WHERE c.relkind = 'r' AND s.kind = ANY(?)`, kinds).Scan(&n)
	return n, err
}

// ScratchDir is backup's scratch directory under STATE_DIR: the SQLite copies a backup
// exports and validates live there.
const ScratchDir = "backup-scratch"

// Held reports whether the database at path lives in Postgres: the rule for every open
// of an existing database. On a Postgres install every path under STATE_DIR and
// CONFIG_DIR is Postgres, catalogued or not, so no live database is ever read from a
// SQLite file left beside it; paths outside them (restore staging, capsule members,
// snapshots) are SQLite files. A test schema path is Postgres when catalogued. Any
// catalog or configuration error is returned: callers fail closed, never fall back.
func Held(path string) (bool, error) {
	if !Postgres() {
		return false, nil
	}
	if s := current(); s.err != nil {
		return false, s.err
	}
	_, rel, err := SchemaName(path)
	switch {
	case err != nil, rel == "state/"+ScratchDir || strings.HasPrefix(rel, "state/"+ScratchDir+"/"):
		return false, nil // outside the data roots, or backup scratch: SQLite files
	case strings.HasPrefix(rel, "state/") || strings.HasPrefix(rel, "config/"):
		return true, nil
	}
	return Exists(path)
}

// Present reports whether the database at path exists: a catalogued schema, or a regular
// file where Held says SQLite. An error reads as absent.
func Present(path string) bool {
	if pg, err := Held(path); err != nil {
		return false
	} else if pg {
		held, err := Exists(path)
		return err == nil && held
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// ErrLiveDatabase refuses a staging-only SQLite helper pointed at a live Postgres store.
var ErrLiveDatabase = errors.New("this check runs on restore staging or a capsule's SQLite files, not on the live Postgres database")

// SQLiteOnly is the guard of the helpers that open restore staging and capsule members
// as SQLite files directly: a path Held says is Postgres, or an error deciding it, is
// refused, so none of them can answer from a file left at a live path.
func SQLiteOnly(path string) error {
	pg, err := Held(path)
	if err != nil {
		return err
	}
	if pg {
		return ErrLiveDatabase
	}
	return nil
}
