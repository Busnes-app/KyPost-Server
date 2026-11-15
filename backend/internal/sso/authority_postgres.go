package sso

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// The v3 authority on Postgres: the same tables, keys, indexes and invariants as
// authoritySchemaV3, the triggers as plpgsql. sqliteTx runs on a transaction of the
// schema's pool. Restore staging and snapshots stay SQLite files. The lifecycle keys join
// their parts with NUL, which Postgres text cannot hold, so those key columns are BYTEA
// (pgBytesKey): written as bytes, read back as strings (queryRows).
const authorityPGSchema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS directory(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision BIGINT NOT NULL CHECK(revision >= 0), digest TEXT NOT NULL,
  active BIGINT NOT NULL CHECK(active IN (0,1)), event_id TEXT NOT NULL,
  revoked_before BIGINT NOT NULL DEFAULT 0,
  resource TEXT,
  PRIMARY KEY(issuer, subject));
CREATE TABLE IF NOT EXISTS directory_primary_emails(
  fold TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL,
  PRIMARY KEY(fold, issuer, subject));
CREATE TABLE IF NOT EXISTS accounts(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision BIGINT NOT NULL, digest TEXT NOT NULL, desired_active BIGINT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','applied','failed')), failure TEXT NOT NULL DEFAULT '',
  primary_mailbox TEXT NOT NULL UNIQUE,
  legacy_mixed_use BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY(issuer, subject));
CREATE TABLE IF NOT EXISTS mailboxes(
  id TEXT PRIMARY KEY,
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('primary','extra')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled')),
  state_root TEXT NOT NULL, message_bytes BIGINT NOT NULL, payload_bytes BIGINT NOT NULL,
  records BIGINT NOT NULL, source TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(issuer, subject) REFERENCES accounts DEFERRABLE INITIALLY DEFERRED);
CREATE INDEX IF NOT EXISTS mailboxes_owner ON mailboxes(issuer, subject);
CREATE TABLE IF NOT EXISTS addresses(
  address TEXT PRIMARY KEY CHECK(address = lower(address)),
  mailbox TEXT NOT NULL REFERENCES mailboxes(id),
  kind TEXT NOT NULL CHECK(kind IN ('primary','alias')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled','reserved') AND (kind = 'alias' OR state <> 'reserved')),
  generation BIGINT NOT NULL CHECK(generation >= 1));
CREATE UNIQUE INDEX IF NOT EXISTS addresses_one_primary ON addresses(mailbox) WHERE kind = 'primary';
CREATE INDEX IF NOT EXISTS addresses_mailbox ON addresses(mailbox);
CREATE TABLE IF NOT EXISTS address_history(
  address TEXT NOT NULL REFERENCES addresses, generation BIGINT NOT NULL CHECK(generation >= 1),
  mailbox TEXT NOT NULL, PRIMARY KEY(address, generation));
CREATE TABLE IF NOT EXISTS routes_pending(address TEXT PRIMARY KEY, generation BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS directory_events(key BYTEA PRIMARY KEY, value TEXT NOT NULL, expires BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS directory_events_expiry ON directory_events(expires);
CREATE TABLE IF NOT EXISTS logouts(key BYTEA PRIMARY KEY, value TEXT NOT NULL, expires BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS logouts_expiry ON logouts(expires);
CREATE TABLE IF NOT EXISTS sign_ons(key BYTEA PRIMARY KEY, expires BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS sign_ons_expiry ON sign_ons(expires);
CREATE TABLE IF NOT EXISTS release_floors(key BYTEA PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS recovery_floors(key BYTEA PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS recovery_barriers(key BYTEA PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS lifecycle_records(
  name TEXT PRIMARY KEY CHECK(name IN ('recoveryRepair','recoveryChallenge','recoveryReceipt','restoreRelease')),
  json TEXT NOT NULL);

CREATE OR REPLACE FUNCTION authority_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '%', TG_ARGV[0]; END $$;
CREATE OR REPLACE TRIGGER accounts_no_delete BEFORE DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION authority_abort('native authority rows are never deleted');
CREATE OR REPLACE TRIGGER mailboxes_no_delete BEFORE DELETE ON mailboxes FOR EACH ROW EXECUTE FUNCTION authority_abort('native authority rows are never deleted');
CREATE OR REPLACE TRIGGER addresses_no_delete BEFORE DELETE ON addresses FOR EACH ROW EXECUTE FUNCTION authority_abort('native authority rows are never deleted');
CREATE OR REPLACE TRIGGER address_history_no_delete BEFORE DELETE ON address_history FOR EACH ROW EXECUTE FUNCTION authority_abort('native authority rows are never deleted');
CREATE OR REPLACE TRIGGER address_history_no_update BEFORE UPDATE ON address_history FOR EACH ROW EXECUTE FUNCTION authority_abort('address history is append-only');
CREATE OR REPLACE FUNCTION address_history_increases() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NEW.generation <= COALESCE((SELECT max(generation) FROM address_history WHERE address = NEW.address), 0) THEN
    RAISE EXCEPTION 'address history generations only increase';
  END IF;
  RETURN NEW; END $$;
CREATE OR REPLACE TRIGGER address_history_increases BEFORE INSERT ON address_history FOR EACH ROW EXECUTE FUNCTION address_history_increases();
CREATE OR REPLACE TRIGGER addresses_generation_no_decrease BEFORE UPDATE OF generation ON addresses FOR EACH ROW
  WHEN (NEW.generation < OLD.generation) EXECUTE FUNCTION authority_abort('address generations never decrease');
CREATE OR REPLACE TRIGGER addresses_identity BEFORE UPDATE OF address ON addresses FOR EACH ROW
  WHEN (NEW.address IS DISTINCT FROM OLD.address) EXECUTE FUNCTION authority_abort('address identity is immutable');
CREATE OR REPLACE TRIGGER mailboxes_identity BEFORE UPDATE OF id, issuer, subject, kind, state_root, source ON mailboxes FOR EACH ROW
  WHEN (NEW.id IS DISTINCT FROM OLD.id OR NEW.issuer IS DISTINCT FROM OLD.issuer OR NEW.subject IS DISTINCT FROM OLD.subject OR NEW.kind IS DISTINCT FROM OLD.kind
    OR NEW.state_root IS DISTINCT FROM OLD.state_root OR OLD.source <> '' AND NEW.source IS DISTINCT FROM OLD.source)
  EXECUTE FUNCTION authority_abort('mailbox identity is immutable');
CREATE OR REPLACE TRIGGER accounts_identity BEFORE UPDATE OF issuer, subject, primary_mailbox ON accounts FOR EACH ROW
  WHEN (NEW.issuer IS DISTINCT FROM OLD.issuer OR NEW.subject IS DISTINCT FROM OLD.subject OR NEW.primary_mailbox IS DISTINCT FROM OLD.primary_mailbox)
  EXECUTE FUNCTION authority_abort('account identity is immutable');
CREATE OR REPLACE FUNCTION accounts_primary_mailbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF NOT EXISTS(SELECT 1 FROM mailboxes WHERE id = NEW.primary_mailbox AND kind = 'primary' AND issuer = NEW.issuer AND subject = NEW.subject) THEN
    RAISE EXCEPTION 'an account names its own primary mailbox';
  END IF;
  RETURN NEW; END $$;
CREATE OR REPLACE TRIGGER accounts_primary_mailbox BEFORE INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION accounts_primary_mailbox();
`

// AuthorityKind is the catalog kind of native-authority.db.
const AuthorityKind = "native-authority"

func init() {
	dbbackend.Register(dbbackend.Kind{Name: AuthorityKind, File: NativeAuthorityFile, DDL: authorityPGSchema, CreateSQLite: func(path string) error {
		if _, err := os.Lstat(path); err == nil {
			return nil // v3 is the only database format
		}
		return writeAuthorityV3(context.Background(), path, authoritySchemaV3, authorityRows{})
	}})
}

// onPostgres reports whether the authority at path is a Postgres schema (dbbackend.Held):
// the live store of a Postgres install. Restore staging and snapshots are SQLite files.
func onPostgres(path string) bool { return dbbackend.Held(path) }

// authorityExists is os.Lstat of the database: a held schema or a file.
func authorityExists(path string) (bool, error) {
	if onPostgres(path) {
		return true, nil
	}
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// pgAuthorityFingerprint is the schema authorityPGSchema builds; opening requires it
// exactly, as the SQLite store requires authoritySchemaV3 (finding S3).
var pgAuthorityFingerprint = sync.OnceValues(func() ([]string, error) {
	return dbbackend.DDLFingerprint(context.Background(), authorityPGSchema)
})

var pgAuthorityPools = struct {
	sync.Mutex
	m map[string]*sql.DB
}{m: map[string]*sql.DB{}}

// pgAuthorityPool is the held, verified pool on the schema for path.
func pgAuthorityPool(ctx context.Context, path string) (*sql.DB, error) {
	pgAuthorityPools.Lock()
	defer pgAuthorityPools.Unlock()
	if db := pgAuthorityPools.m[path]; db != nil {
		return db, nil
	}
	db, err := dbbackend.Open(path, dbbackend.Options{Kind: AuthorityKind})
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNativeMigration
	} else if err != nil {
		return nil, authorityErr(err)
	}
	want, err := pgAuthorityFingerprint()
	if err == nil {
		var got []string
		if got, err = dbbackend.Fingerprint(ctx, db); err == nil && !slices.Equal(got, want) {
			err = ErrNativeMigration
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, authorityErr(err)
	}
	pgAuthorityPools.m[path] = db
	return db, nil
}

// dropPGAuthorityPool forgets the pool on path (the schema was replaced or dropped).
func dropPGAuthorityPool(path string) {
	pgAuthorityPools.Lock()
	defer pgAuthorityPools.Unlock()
	if db := pgAuthorityPools.m[path]; db != nil {
		_ = db.Close()
		delete(pgAuthorityPools.m, path)
	}
}

// runPostgres is run on Postgres: a View is a READ ONLY REPEATABLE READ snapshot, as a
// SQLite read transaction is; an Update holds the schema's writer lock, as BEGIN
// IMMEDIATE does.
func (a sqliteAuthority) runPostgres(ctx context.Context, write bool, pre *subjectRows, fn func(*sqliteTx) error) (*sqliteTx, error) {
	db, err := pgAuthorityPool(ctx, a.path)
	if err != nil {
		return nil, err
	}
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	if write {
		opts = nil
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, authorityErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	t := &sqliteTx{ctx: ctx, c: tx, pre: pre, postgres: true}
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
	if err := tx.Commit(); err != nil {
		return nil, authorityErr(err)
	}
	return t, nil
}

// buildAuthorityPostgres is buildAuthority on Postgres: the schema with its rows in one
// transaction, replacing any earlier one (only a fresh install or migrate-native gets
// here), read back and checked before the commit.
func buildAuthorityPostgres(ctx context.Context, path string, rows authorityRows, check func(lifecycleFile, *nativeLedger) error) error {
	dropPGAuthorityPool(path)
	schema, rel, err := dbbackend.SchemaName(path)
	if err != nil {
		return err
	}
	tx, err := dbbackend.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+dbbackend.QuoteIdent(schema)+` CASCADE; DELETE FROM kypost_catalog.stores WHERE path='`+strings.ReplaceAll(rel, "'", "''")+`'`); err != nil {
		return err
	}
	if err := dbbackend.CreateSchemaTx(ctx, tx, schema, rel, AuthorityKind, authorityPGSchema); err != nil {
		return err
	}
	for _, table := range authorityTablesV3 {
		for _, row := range rows[table] {
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")+")", pgRow(true, table, row)...); err != nil {
				return err
			}
		}
	}
	if err := verifyAuthorityRows(ctx, tx, check); err != nil {
		return err
	}
	return tx.Commit()
}

// readAuthorityPostgres is readAuthorityFile on Postgres.
func readAuthorityPostgres(ctx context.Context, path string) (life lifecycleFile, l *nativeLedger, err error) {
	db, err := pgAuthorityPool(ctx, path)
	if err != nil {
		return life, nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return life, nil, authorityErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	err = verifyAuthorityRows(ctx, tx, func(gotLife lifecycleFile, gotLedger *nativeLedger) error {
		life, l = gotLife, gotLedger
		return nil
	})
	return life, l, err
}

// verifyAuthorityRows reads every table and validates all of it, as verifyAuthorityFile
// does after its integrity checks (Postgres enforces the keys itself).
func verifyAuthorityRows(ctx context.Context, q queryer, check func(lifecycleFile, *nativeLedger) error) error {
	r := authorityRows{}
	for _, table := range authorityTablesV3 {
		if err := readTable(ctx, q, table, r); err != nil {
			return authorityErr(err)
		}
	}
	life, l, err := r.decode()
	if err != nil {
		return err
	}
	if l != nil {
		if _, _, err := validateNativeLedger(*l); err != nil {
			return err
		}
	}
	if check != nil {
		return check(life, l)
	}
	return nil
}

// pgBytesKey are the tables whose first column, key, is BYTEA on Postgres.
var pgBytesKey = map[string]bool{"directory_events": true, "logouts": true, "sign_ons": true, "release_floors": true, "recovery_floors": true, "recovery_barriers": true}

// pgRow is row with its key as bytes where the table keeps it BYTEA on Postgres.
func pgRow(postgres bool, table string, row []any) []any {
	if !postgres || !pgBytesKey[table] || len(row) == 0 {
		return row
	}
	out := slices.Clone(row)
	if k, ok := out[0].(string); ok {
		out[0] = []byte(k)
	}
	return out
}
