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
	pg, err := dbbackend.Postgres(), error(nil)
	if !create {
		if pg, err = dbbackend.Held(path); err != nil {
			return nil, err
		}
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
