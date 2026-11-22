// Package tenancy holds the multi-tenant install record and side tables
// (docs/superpowers/specs/2026-11-20-tenancy-design.md, K1-K4). A single-tenant install
// never writes any of it.
package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	_ "modernc.org/sqlite"
)

// File is the multi-only store in STATE_DIR, Kind its catalog kind. The side tables live
// here rather than in state.db and native-authority.db, whose copies and exact-schema
// checks would refuse an extra table.
const (
	File = "tenancy.db"
	Kind = "tenancy"
)

// The entitlement cache (orgs, K8) and the two org roots (K3/K4). user_orgs and
// subject_orgs are write-once: an UPDATE is refused, and writers use plain INSERT.
// receiving_gateways holds the MX hosts' pinned keys (gateways.go); its rows are never
// deleted and only gain revoked_at and retired_at. sender_blocks and sender_evidence are
// the multi-mode sender block list and its evidence (ingress.TableSenders). org_purges,
// its org_purge_members snapshot and the never-changed address_tombstones are the purge's
// (purge.go).
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS orgs (
 id TEXT PRIMARY KEY,
 status TEXT NOT NULL CHECK(status IN ('active','suspended','deletion_scheduled','deleted')),
 revision INTEGER NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 quotas_json TEXT NOT NULL, polled_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS user_orgs (user_id TEXT PRIMARY KEY, org_id TEXT NOT NULL CHECK(org_id <> ''));
CREATE TABLE IF NOT EXISTS subject_orgs (
 issuer TEXT NOT NULL, subject TEXT NOT NULL, org_id TEXT NOT NULL CHECK(org_id <> ''),
 PRIMARY KEY(issuer, subject));
CREATE TRIGGER IF NOT EXISTS user_orgs_write_once BEFORE UPDATE ON user_orgs BEGIN SELECT RAISE(ABORT, 'org bindings are write-once'); END;
CREATE TRIGGER IF NOT EXISTS subject_orgs_write_once BEFORE UPDATE ON subject_orgs BEGIN SELECT RAISE(ABORT, 'org bindings are write-once'); END;
CREATE TABLE IF NOT EXISTS receiving_gateways (
 id TEXT NOT NULL, spki_sha256 TEXT NOT NULL UNIQUE,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked_at INTEGER, retired_at INTEGER,
 PRIMARY KEY(id, spki_sha256));
CREATE TRIGGER IF NOT EXISTS receiving_gateways_kept BEFORE DELETE ON receiving_gateways BEGIN SELECT RAISE(ABORT, 'gateway pins are never deleted'); END;
CREATE TRIGGER IF NOT EXISTS receiving_gateways_final BEFORE UPDATE ON receiving_gateways
 WHEN NEW.id IS NOT OLD.id OR NEW.spki_sha256 IS NOT OLD.spki_sha256 OR NEW.created_at IS NOT OLD.created_at
  OR NEW.expires_at IS NOT OLD.expires_at OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NOT OLD.revoked_at)
  OR (OLD.retired_at IS NOT NULL AND NEW.retired_at IS NOT OLD.retired_at)
 BEGIN SELECT RAISE(ABORT, 'a gateway pin only ever gains revoked_at and retired_at'); END;
CREATE TABLE IF NOT EXISTS sender_blocks (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('address','domain')), value TEXT NOT NULL, until INTEGER,
 source TEXT NOT NULL CHECK(source IN ('manual','automatic')), level INTEGER NOT NULL, created_at INTEGER NOT NULL,
 actor TEXT NOT NULL, reason TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sender_evidence (
 kind TEXT NOT NULL CHECK(kind IN ('address','domain','good','unblocked')), id TEXT NOT NULL, ref TEXT NOT NULL,
 hits TEXT NOT NULL, level INTEGER NOT NULL, last INTEGER NOT NULL, blocked INTEGER NOT NULL, PRIMARY KEY(kind, id));
CREATE INDEX IF NOT EXISTS sender_evidence_ref ON sender_evidence(kind, ref, blocked);
CREATE INDEX IF NOT EXISTS sender_evidence_last ON sender_evidence(kind, last);
CREATE TABLE IF NOT EXISTS org_purges (
 org_id TEXT NOT NULL, subject TEXT NOT NULL, step INTEGER NOT NULL, started_at INTEGER NOT NULL, done_at INTEGER,
 PRIMARY KEY(org_id, subject));
CREATE TABLE IF NOT EXISTS org_purge_members (
 org_id TEXT NOT NULL, subject TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('user','mailbox','subject')),
 issuer TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY(org_id, subject, kind, issuer, id));
CREATE TABLE IF NOT EXISTS address_tombstones (hmac TEXT PRIMARY KEY);
CREATE TRIGGER IF NOT EXISTS address_tombstones_kept BEFORE DELETE ON address_tombstones BEGIN SELECT RAISE(ABORT, 'address tombstones are never deleted'); END;
CREATE TRIGGER IF NOT EXISTS address_tombstones_final BEFORE UPDATE ON address_tombstones BEGIN SELECT RAISE(ABORT, 'address tombstones are never changed'); END;
`

const pgSchema = `
CREATE TABLE IF NOT EXISTS orgs (
 id TEXT PRIMARY KEY,
 status TEXT NOT NULL CHECK(status IN ('active','suspended','deletion_scheduled','deleted')),
 revision BIGINT NOT NULL, enabled BIGINT NOT NULL CHECK(enabled IN (0,1)),
 quotas_json TEXT NOT NULL, polled_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS user_orgs (user_id TEXT PRIMARY KEY, org_id TEXT NOT NULL CHECK(org_id <> ''));
CREATE TABLE IF NOT EXISTS subject_orgs (
 issuer TEXT NOT NULL, subject TEXT NOT NULL, org_id TEXT NOT NULL CHECK(org_id <> ''),
 PRIMARY KEY(issuer, subject));
CREATE OR REPLACE FUNCTION org_binding_write_once() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'org bindings are write-once'; END $$;
CREATE OR REPLACE TRIGGER user_orgs_write_once BEFORE UPDATE ON user_orgs FOR EACH ROW EXECUTE FUNCTION org_binding_write_once();
CREATE OR REPLACE TRIGGER subject_orgs_write_once BEFORE UPDATE ON subject_orgs FOR EACH ROW EXECUTE FUNCTION org_binding_write_once();
CREATE TABLE IF NOT EXISTS receiving_gateways (
 id TEXT NOT NULL, spki_sha256 TEXT NOT NULL UNIQUE,
 created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, revoked_at BIGINT, retired_at BIGINT,
 PRIMARY KEY(id, spki_sha256));
CREATE OR REPLACE FUNCTION receiving_gateway_final() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'gateway pins are never deleted'; END IF;
 IF NEW.id IS DISTINCT FROM OLD.id OR NEW.spki_sha256 IS DISTINCT FROM OLD.spki_sha256 OR NEW.created_at IS DISTINCT FROM OLD.created_at
  OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
  OR (OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at) THEN
  RAISE EXCEPTION 'a gateway pin only ever gains revoked_at and retired_at'; END IF;
 RETURN NEW; END $$;
CREATE OR REPLACE TRIGGER receiving_gateways_final BEFORE UPDATE OR DELETE ON receiving_gateways FOR EACH ROW EXECUTE FUNCTION receiving_gateway_final();
CREATE TABLE IF NOT EXISTS sender_blocks (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('address','domain')), value TEXT NOT NULL, until BIGINT,
 source TEXT NOT NULL CHECK(source IN ('manual','automatic')), level BIGINT NOT NULL, created_at BIGINT NOT NULL,
 actor TEXT NOT NULL, reason TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sender_evidence (
 kind TEXT NOT NULL CHECK(kind IN ('address','domain','good','unblocked')), id TEXT NOT NULL, ref TEXT NOT NULL,
 hits TEXT NOT NULL, level BIGINT NOT NULL, last BIGINT NOT NULL, blocked BIGINT NOT NULL, PRIMARY KEY(kind, id));
CREATE INDEX IF NOT EXISTS sender_evidence_ref ON sender_evidence(kind, ref, blocked);
CREATE INDEX IF NOT EXISTS sender_evidence_last ON sender_evidence(kind, last);
CREATE TABLE IF NOT EXISTS org_purges (
 org_id TEXT NOT NULL, subject TEXT NOT NULL, step BIGINT NOT NULL, started_at BIGINT NOT NULL, done_at BIGINT,
 PRIMARY KEY(org_id, subject));
CREATE TABLE IF NOT EXISTS org_purge_members (
 org_id TEXT NOT NULL, subject TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('user','mailbox','subject')),
 issuer TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY(org_id, subject, kind, issuer, id));
CREATE TABLE IF NOT EXISTS address_tombstones (hmac TEXT PRIMARY KEY);
CREATE OR REPLACE FUNCTION address_tombstone_kept() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'address tombstones are never deleted or changed'; END $$;
CREATE OR REPLACE TRIGGER address_tombstones_kept BEFORE UPDATE OR DELETE ON address_tombstones FOR EACH ROW EXECUTE FUNCTION address_tombstone_kept();
`

func init() {
	dbbackend.Register(dbbackend.Kind{Name: Kind, File: File, DDL: pgSchema, CreateSQLite: func(path string) error {
		db, err := openSQLite(path, true)
		if err == nil {
			err = db.Close()
		}
		return err
	}})
}

// Open opens the store in stateDir. Only a multi install's first start creates it; any
// other open of a missing store is os.ErrNotExist.
func Open(stateDir string, create bool) (*sql.DB, error) {
	path := filepath.Join(stateDir, File)
	pg := dbbackend.Postgres()
	if !create {
		held, err := dbbackend.Held(path)
		if err != nil {
			return nil, err
		}
		pg = held
	}
	if !pg {
		return openSQLite(path, create)
	}
	db, err := dbbackend.Open(path, dbbackend.Options{Kind: Kind, DDL: pgSchema, Create: create})
	if errors.Is(err, dbbackend.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, File)
	}
	return db, err
}

func openSQLite(path string, create bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !create {
		if _, err := os.Lstat(abs); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs}).String()+
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Errors of Check. None names a value: the staff org is an identifier we do not log.
var (
	ErrNotEmpty      = errors.New("KYPOST_TENANCY=multi needs an empty install at its first start, and this one already holds data")
	ErrModeMismatch  = errors.New("this install is multi-tenant: start it with KYPOST_TENANCY=multi and its KYPOST_STAFF_ORG_ID")
	ErrStaffMismatch = errors.New("KYPOST_STAFF_ORG_ID differs from this install's staff org")
	ErrDamaged       = errors.New("this install's tenancy record is damaged; preserve the database and repair it")
)

// Check applies K1 at start. With no record, single mode writes nothing and multi mode, on
// an empty install, creates the side tables and then the record, under the replica-config
// lock so concurrent first starts write it once. A record refuses a start with another
// mode or staff org. empty is consulted only on a multi first start.
func Check(ctx context.Context, st *state.Store, stateDir, mode, staffOrg string, empty func() (bool, error)) error {
	if mode != config.TenancyMulti {
		recMode, recStaff, err := st.TenancyRecord()
		switch {
		case err != nil:
			return fmt.Errorf("tenancy check: %w", err)
		case recMode != "" || recStaff != "":
			return ErrModeMismatch
		}
		return nil
	}
	return st.Serialized(ctx, "replica-config", func(context.Context) error {
		recMode, recStaff, err := st.TenancyRecord()
		switch {
		case err != nil:
			return fmt.Errorf("tenancy check: %w", err)
		case recMode == "" && recStaff == "":
			ok, err := empty()
			if err != nil {
				return fmt.Errorf("tenancy check: %w", err)
			}
			if !ok {
				return ErrNotEmpty
			}
			db, err := Open(stateDir, true)
			if err != nil {
				return fmt.Errorf("tenancy check: %w", err)
			}
			_ = db.Close()
			return st.RecordTenancy(mode, staffOrg)
		case recMode != config.TenancyMulti || !config.ValidOrgID(recStaff):
			return ErrDamaged
		case recStaff != staffOrg:
			return ErrStaffMismatch
		}
		db, err := Open(stateDir, false)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDamaged, err)
		}
		return db.Close()
	})
}

// Empty reports whether an install holds no data yet: no accounts, no per-user or
// per-mailbox directory, and no native authority or receiving buffer.
func Empty(accounts int, configDir, stateDir string) (bool, error) {
	if accounts > 0 {
		return false, nil
	}
	for _, dir := range []string{filepath.Join(configDir, "users"), filepath.Join(stateDir, "users"), filepath.Join(stateDir, "mailboxes")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if len(entries) > 0 {
			return false, nil
		}
	}
	for _, path := range []string{filepath.Join(configDir, "native-authority.db"), filepath.Join(stateDir, "receiving", "ingress.db")} {
		pg, err := dbbackend.Held(path)
		if err != nil {
			return false, err
		}
		held := false
		if pg {
			held, err = dbbackend.Exists(path)
		} else if _, err = os.Lstat(path); err == nil {
			held = true
		} else if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil || held {
			return false, err
		}
	}
	return true, nil
}

// ErrOrgMismatch refuses a binding that already names another org. It names no value.
var ErrOrgMismatch = errors.New("the account is bound to another org")

// BindSubject records a KyIdentity subject's org once (K "Identity binding", step 1).
// A repeat with the same org is a no-op; another org is ErrOrgMismatch and changes nothing.
func BindSubject(ctx context.Context, db *sql.DB, issuer, subject, org string) error {
	return bind(ctx, db, org, `INSERT INTO subject_orgs(issuer, subject, org_id) VALUES(?, ?, ?) ON CONFLICT DO NOTHING`,
		`SELECT org_id FROM subject_orgs WHERE issuer = ? AND subject = ?`, issuer, subject)
}

// BindUser records an account's org once (step 3), as BindSubject does.
func BindUser(ctx context.Context, db *sql.DB, userID, org string) error {
	return bind(ctx, db, org, `INSERT INTO user_orgs(user_id, org_id) VALUES(?, ?) ON CONFLICT DO NOTHING`,
		`SELECT org_id FROM user_orgs WHERE user_id = ?`, userID)
}

// ON CONFLICT DO NOTHING never replaces a row, unlike SQLite's INSERT OR REPLACE, so an
// existing binding stays and the comparison below decides.
func bind(ctx context.Context, db *sql.DB, org, insert, query string, key ...any) error {
	if db == nil || !config.ValidOrgID(org) {
		return ErrOrgMismatch
	}
	if _, err := db.ExecContext(ctx, insert, append(key, org)...); err != nil {
		return err
	}
	got, err := lookup(ctx, db, query, key...)
	if err == nil && got != org {
		err = ErrOrgMismatch
	}
	return err
}

// SubjectOrg returns the subject's recorded org, "" when it has none.
func SubjectOrg(ctx context.Context, db *sql.DB, issuer, subject string) (string, error) {
	return lookup(ctx, db, `SELECT org_id FROM subject_orgs WHERE issuer = ? AND subject = ?`, issuer, subject)
}

// KnownOrg reports whether the entitlement cache (orgs, K8) has a row for org, in any
// status. Staff select an org only from the cache; the staff org is exempt (K8).
func KnownOrg(ctx context.Context, db *sql.DB, org string) (bool, error) {
	id, err := lookup(ctx, db, `SELECT id FROM orgs WHERE id = ?`, org)
	return id != "", err
}

func lookup(ctx context.Context, db *sql.DB, query string, key ...any) (string, error) {
	if db == nil {
		return "", ErrOrgMismatch
	}
	var org string
	err := db.QueryRowContext(ctx, query, key...).Scan(&org)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return org, err
}
