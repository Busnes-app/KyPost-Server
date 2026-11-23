package users

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
	"github.com/jackc/pgx/v5/pgconn"
)

// Account mounts are installed once during table-mode startup. File-only starts
// install none; every existing constructor continues to resolve its config path.
type accountMount struct {
	db       *sql.DB
	orgTable string
}

var accountMounts struct {
	sync.RWMutex
	entries map[string]*accountMount
}

// MountAccounts selects an already opened volumes.db for one configuration root.
// The caller owns db and removes the mount only after its stores have stopped.
func MountAccounts(configDir, stateDir string) (func(), error) {
	db, err := volumes.OpenAccounts(stateDir)
	if err != nil {
		return nil, err
	}
	release, err := mountAccounts(configDir, stateDir, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return func() { release(); _ = db.Close() }, nil
}

func mountAccounts(configDir, stateDir string, db *sql.DB) (func(), error) {
	if db == nil {
		return nil, errors.New("accounts need a volumes database")
	}
	root, err := filepath.Abs(configDir)
	if err != nil {
		return nil, err
	}
	accountMounts.Lock()
	defer accountMounts.Unlock()
	if accountMounts.entries == nil {
		accountMounts.entries = make(map[string]*accountMount)
	}
	if accountMounts.entries[root] != nil {
		return nil, errors.New("accounts root is already mounted")
	}
	schema, _, err := dbbackend.SchemaName(filepath.Join(stateDir, "tenancy.db"))
	if err != nil {
		return nil, err
	}
	accountMounts.entries[root] = &accountMount{db: db, orgTable: `"` + schema + `".orgs`}
	return func() { accountMounts.Lock(); delete(accountMounts.entries, root); accountMounts.Unlock() }, nil
}

func mountedAccounts(path string) *accountMount {
	root, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil
	}
	accountMounts.RLock()
	defer accountMounts.RUnlock()
	return accountMounts.entries[root]
}

type accountReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// readAccounts validates the SQL routing columns against the actual record.
// A corrupt binding is an error, never an absent or unbound account.
func readAccounts(ctx context.Context, db accountReader, suffix string, args ...any) (usersFile, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,username,issuer,subject,record FROM accounts `+suffix, args...)
	if err != nil {
		return usersFile{}, err
	}
	defer rows.Close()
	f := usersFile{Version: 1, Users: []User{}}
	for rows.Next() {
		var id, name string
		var issuer, subject sql.NullString
		var raw []byte
		if err := rows.Scan(&id, &name, &issuer, &subject, &raw); err != nil {
			return usersFile{}, err
		}
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return usersFile{}, errors.New("invalid account record")
		}
		if id == "" || u.ID != id || NormalizeUsername(u.Username) != name || u.SSOIssuer != issuer.String || u.SSOSub != subject.String || issuer.Valid != subject.Valid || issuer.Valid && (issuer.String == "" || subject.String == "") {
			return usersFile{}, errors.New("account routing columns disagree with the record")
		}
		f.Users = append(f.Users, u)
	}
	return f, rows.Err()
}

func (s *Store) accountSnapshot(ctx context.Context, action func([]User) error) error {
	tx, err := s.accounts.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	f, err := readAccounts(ctx, tx, "ORDER BY id")
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = action(f.Users); err != nil {
		return err
	}
	return tx.Commit()
}

func serializationFailure(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40001"
}

// All writers use SERIALIZABLE, including whole-set administrator checks. The
// bounded retry reruns the complete read/check/write, never only its last SQL.
// The store mutex owns activeAccounts for the duration of every attempt.
func (s *Store) accountWrite(ctx context.Context, operation func() error) error {
	return s.accountWriteAfter(ctx, operation, nil)
}

func (s *Store) accountWriteAfter(ctx context.Context, operation, after func() error) error {
	for range 5 {
		committed, err := func() (bool, error) {
			tx, err := s.accounts.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
			if err != nil {
				return false, err
			}
			defer func() { _ = tx.Rollback() }()
			s.activeAccounts, s.accountsContext = tx, ctx
			defer func() {
				for i := len(s.accountsReleases) - 1; i >= 0; i-- {
					s.accountsReleases[i]()
				}
				s.accountsReleases = nil
				s.activeAccounts = nil
				s.accountsContext = nil
			}()
			if err = operation(); err != nil {
				return false, err
			}
			if err = tx.Commit(); err != nil {
				return false, err
			}
			if after != nil {
				return true, after()
			}
			return true, nil
		}()
		// Never replay an external finalizer after accounts have committed.
		if committed || !serializationFailure(err) {
			return accountError(err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("account write contention: %w", ErrKDFBusy)
}

func (s *Store) withWriteLockAndCommit(ctx context.Context, operation, commit func() error) error {
	if s.accounts != nil {
		return s.accountWriteAfter(ctx, operation, commit)
	}
	return s.withWriteLock(ctx, func() error {
		defer func() {
			for i := len(s.accountsReleases) - 1; i >= 0; i-- {
				s.accountsReleases[i]()
			}
			s.accountsReleases = nil
		}()
		if err := operation(); err != nil {
			return err
		}
		return commit()
	})
}

func accountError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		switch pg.ConstraintName {
		case "accounts_username_key":
			return ErrUsernameTaken
		case "accounts_issuer_subject_key":
			return ErrSSOSubTaken
		case "accounts_pkey":
			return ErrNativeAccountConflict
		}
	}
	return err
}

// writeAccounts compares the prior records in the same transaction and updates
// only changed rows. Epoch and org invariants are checked by the common writer.
func (s *Store) writeAccounts(f usersFile) error {
	if s.activeAccounts == nil {
		return errors.New("account write needs its transaction")
	}
	old, err := readAccounts(s.accountsContext, s.activeAccounts, "ORDER BY id")
	if err != nil {
		return err
	}
	before := make(map[string][]byte, len(old.Users))
	for _, u := range old.Users {
		raw, e := json.Marshal(u)
		if e != nil {
			return e
		}
		before[u.ID] = raw
	}
	seen := make(map[string]bool, len(f.Users))
	for _, u := range f.Users {
		if u.ID == "" || seen[u.ID] || (u.SSOIssuer == "") != (u.SSOSub == "") {
			return ErrNativeAccountConflict
		}
		seen[u.ID] = true
		raw, e := json.Marshal(u)
		if e != nil {
			return e
		}
		prior, exists := before[u.ID]
		if exists && bytes.Equal(prior, raw) {
			continue
		}
		var issuer, subject any
		if u.SSOIssuer != "" {
			issuer, subject = u.SSOIssuer, u.SSOSub
		}
		if !exists && u.OrgID != "" && u.OrgID != s.StaffOrg {
			// Status changes and publication share a database row lock, even after
			// the cross-store org fence is lost. A SERIALIZABLE stale read retries.
			var status string
			if err = s.activeAccounts.QueryRowContext(s.accountsContext, `SELECT status FROM `+s.orgTable+` WHERE id=? FOR SHARE`, u.OrgID).Scan(&status); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNativeAccountConflict
				}
				return err
			}
			if status == "deleted" {
				return ErrNativeAccountConflict
			}
		}
		if exists {
			var held string
			if err = s.activeAccounts.QueryRowContext(s.accountsContext, `SELECT id FROM accounts WHERE id=? FOR UPDATE`, u.ID).Scan(&held); err != nil {
				return err
			}
			_, err = s.activeAccounts.ExecContext(s.accountsContext, `UPDATE accounts SET username=?,issuer=?,subject=?,record=?,revision=nextval(pg_get_serial_sequence('accounts','revision')) WHERE id=?`, NormalizeUsername(u.Username), issuer, subject, raw, u.ID)
		} else {
			_, err = s.activeAccounts.ExecContext(s.accountsContext, `INSERT INTO accounts(id,username,issuer,subject,record) VALUES(?,?,?,?,?)`, u.ID, NormalizeUsername(u.Username), issuer, subject, raw)
		}
		if err != nil {
			return accountError(err)
		}
	}
	for id := range before {
		if !seen[id] {
			if _, err = s.activeAccounts.ExecContext(s.accountsContext, `DELETE FROM accounts WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) accountLookup(suffix string, args ...any) (User, error) {
	f, err := readAccounts(context.Background(), s.accounts, suffix, args...)
	if err != nil {
		return User{}, err
	}
	if len(f.Users) == 0 {
		return User{}, ErrNotFound
	}
	if len(f.Users) != 1 {
		return User{}, ErrNativeAccountConflict
	}
	return f.Users[0], nil
}

// TableMode is the startup-installed account choice, independent of tenancy mode.
func TableMode(configDir string) bool {
	return mountedAccounts(filepath.Join(configDir, "users.json")) != nil
}

// ReadAccountSnapshot validates and reads private SQLite capsule account rows.
// It does not mount, migrate or write the stopped snapshot.
func ReadAccountSnapshot(ctx context.Context, db *sql.DB) ([]User, error) {
	var version string
	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version); err != nil {
		return nil, errors.New("account snapshots require private SQLite staging")
	}
	f, err := readAccounts(ctx, db, "ORDER BY id")
	return f.Users, err
}
