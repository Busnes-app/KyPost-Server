package tenancy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// The purge's records (K "Purge (`deleted`)", "Delete everything"). One org_purges row per
// scope says a purge started and its last finished step; org_purge_members is the snapshot
// it is driven by, written with the row and deleted when the row is marked done.

// PurgeScope is one purge: a whole org (Subject "") or one erased member's subject.
type PurgeScope struct{ Org, Subject string }

// Subject is a KyIdentity subject under its issuer.
type Subject struct{ Issuer, Subject string }

// PurgeSnapshot is what a purge removes: every step and the final check read it, never
// user_orgs, which step 5 deletes.
type PurgeSnapshot struct {
	Users, Mailboxes []string
	Subjects         []Subject
}

// ErrNoLivePurge refuses the ledger's named trigger exception without a live org_purges row.
var ErrNoLivePurge = errors.New("tenancy: no live purge for this scope")

// StartPurge records scope's row and snapshot in one transaction. A scope that already has
// a row keeps it and its snapshot: started reports whether this call wrote them.
func StartPurge(ctx context.Context, db *sql.DB, scope PurgeScope, snap PurgeSnapshot, now time.Time) (started bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO org_purges(org_id, subject, step, started_at) VALUES(?, ?, 0, ?) ON CONFLICT DO NOTHING`, scope.Org, scope.Subject, now.Unix())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	add := func(kind, issuer, id string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO org_purge_members(org_id, subject, kind, issuer, id) VALUES(?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, scope.Org, scope.Subject, kind, issuer, id)
		return err
	}
	for _, id := range snap.Users {
		if err := add("user", "", id); err != nil {
			return false, err
		}
	}
	for _, id := range snap.Mailboxes {
		if err := add("mailbox", "", id); err != nil {
			return false, err
		}
	}
	for _, s := range snap.Subjects {
		if err := add("subject", s.Issuer, s.Subject); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// PurgeMembers reads scope's snapshot.
func PurgeMembers(ctx context.Context, db *sql.DB, scope PurgeScope) (PurgeSnapshot, error) {
	var snap PurgeSnapshot
	rows, err := db.QueryContext(ctx, `SELECT kind, issuer, id FROM org_purge_members WHERE org_id = ? AND subject = ? ORDER BY kind, issuer, id`, scope.Org, scope.Subject)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, issuer, id string
		if err := rows.Scan(&kind, &issuer, &id); err != nil {
			return snap, err
		}
		switch kind {
		case "user":
			snap.Users = append(snap.Users, id)
		case "mailbox":
			snap.Mailboxes = append(snap.Mailboxes, id)
		default:
			snap.Subjects = append(snap.Subjects, Subject{issuer, id})
		}
	}
	return snap, rows.Err()
}

// LivePurges lists the purges not yet marked done.
func LivePurges(ctx context.Context, db *sql.DB) ([]PurgeScope, error) {
	rows, err := db.QueryContext(ctx, `SELECT org_id, subject FROM org_purges WHERE done_at IS NULL ORDER BY org_id, subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurgeScope
	for rows.Next() {
		var s PurgeScope
		if err := rows.Scan(&s.Org, &s.Subject); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PurgeLive is nil while scope has a row not yet marked done: the key of the ledger's
// named trigger exception.
func PurgeLive(ctx context.Context, db *sql.DB, scope PurgeScope) error {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_purges WHERE org_id = ? AND subject = ? AND done_at IS NULL`, scope.Org, scope.Subject).Scan(&n)
	if err == nil && n == 0 {
		err = ErrNoLivePurge
	}
	return err
}

// Purged reports whether org, or subject in it, has a purge row, live or done: such a
// subject is never provisioned again.
func Purged(ctx context.Context, db *sql.DB, org, subject string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_purges WHERE org_id = ? AND (subject = '' OR subject = ?)`, org, subject).Scan(&n)
	return n > 0, err
}

// MarkPurgeStep records the last finished step; a resumed purge still runs every step.
func MarkPurgeStep(ctx context.Context, db *sql.DB, scope PurgeScope, step int) error {
	_, err := db.ExecContext(ctx, `UPDATE org_purges SET step = ? WHERE org_id = ? AND subject = ? AND done_at IS NULL`, step, scope.Org, scope.Subject)
	return err
}

// FinishPurge marks scope done and deletes its snapshot, in one transaction.
func FinishPurge(ctx context.Context, db *sql.DB, scope PurgeScope, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE org_purges SET done_at = ? WHERE org_id = ? AND subject = ? AND done_at IS NULL`, now.Unix(), scope.Org, scope.Subject); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_purge_members WHERE org_id = ? AND subject = ?`, scope.Org, scope.Subject); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletedOrgs lists the cached orgs whose status is deleted.
func DeletedOrgs(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryColumn(ctx, db, `SELECT id FROM orgs WHERE status = ? ORDER BY id`, suitedir.OrgDeleted)
}

// OrgSubjects lists org's recorded subjects; a non-empty subject limits it to that one.
func OrgSubjects(ctx context.Context, db *sql.DB, org, subject string) ([]Subject, error) {
	rows, err := db.QueryContext(ctx, `SELECT issuer, subject FROM subject_orgs WHERE org_id = ? AND (? = '' OR subject = ?) ORDER BY issuer, subject`, org, subject, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		var s Subject
		if err := rows.Scan(&s.Issuer, &s.Subject); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RemoveUserOrgs deletes the accounts' user_orgs rows (purge step 5).
func RemoveUserOrgs(ctx context.Context, db *sql.DB, ids []string) error {
	for _, id := range ids {
		if _, err := db.ExecContext(ctx, `DELETE FROM user_orgs WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// UserOrgRows counts the accounts' user_orgs rows (the final check).
func UserOrgRows(ctx context.Context, db *sql.DB, ids []string) (int, error) {
	total := 0
	for _, id := range ids {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM user_orgs WHERE user_id = ?`, id).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func queryColumn(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
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

// TombstoneKeyFile is the address tombstones' HMAC key in SECRET_DIR (K10, M open question 4).
const TombstoneKeyFile = "address-tombstone.key"

// ErrTombstoneKeyMissing refuses to mint a new key while tombstones exist: they were made
// under the lost key, and a new one would let every tombstoned address be reissued.
var ErrTombstoneKeyMissing = errors.New("tenancy: address tombstones exist but " + TombstoneKeyFile + " is missing; restore it from the backup")

// TombstoneKey reads the key, creating it exclusively only while no tombstone exists.
func TombstoneKey(ctx context.Context, db *sql.DB, secretDir string) ([]byte, error) {
	path := filepath.Join(secretDir, TombstoneKeyFile)
	key, err := cryptutil.LoadKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	switch n, err := TombstoneCount(ctx, db); {
	case err != nil:
		return nil, err
	case n > 0:
		return nil, ErrTombstoneKeyMissing
	}
	return cryptutil.LoadOrCreateKey(path)
}

// Tombstone is an address's tombstone: an HMAC under key, so no plain address is kept.
func Tombstone(key []byte, address string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.ToLower(strings.TrimSpace(address))))
	return hex.EncodeToString(m.Sum(nil))
}

// AddTombstones records the hashes; a repeat is a no-op.
func AddTombstones(ctx context.Context, db *sql.DB, hashes []string) error {
	for _, h := range hashes {
		if _, err := db.ExecContext(ctx, `INSERT INTO address_tombstones(hmac) VALUES(?) ON CONFLICT DO NOTHING`, h); err != nil {
			return err
		}
	}
	return nil
}

// TombstoneCount counts the address tombstones.
func TombstoneCount(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM address_tombstones`).Scan(&n)
	return n, err
}

// Tombstoned reports whether hash is a tombstone.
func Tombstoned(ctx context.Context, db *sql.DB, hash string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM address_tombstones WHERE hmac = ?`, hash).Scan(&n)
	return n > 0, err
}

// FenceRestored runs on a restore's staged SQLite tenancy.db, before any Postgres import:
// the orgs cache predates the backup's end, so it is emptied and every org is unserved
// until a poll after the restore; finished purges are forgotten, so each deleted org and
// erased subject is purged again once KyIdentity lists it. Live purges keep their
// snapshot and resume. Tombstones and the other tables are kept. No staged tenancy.db
// (every single-tenant capsule) changes nothing.
func FenceRestored(stateDir string) (err error) {
	path, err := filepath.Abs(filepath.Join(stateDir, File))
	if err != nil {
		return err
	}
	if err := dbbackend.SQLiteOnly(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	// A capsule from before PR 6 lacks the purge tables; the schema adds them.
	if _, err := db.Exec(sqliteSchema); err != nil {
		return err
	}
	return ForgetRestored(context.Background(), db)
}

// ForgetRestored is FenceRestored's change: the org cache, the finished purges and any
// restore read go.
func ForgetRestored(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM orgs`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM org_purges WHERE done_at IS NOT NULL`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM restore_reads`)
	return err
}

// RecordRestoreRead records that, during the restore hold epoch, a poll of every org from
// revision 0 and a read of every erased subject both succeeded (K "Delete everything").
func RecordRestoreRead(ctx context.Context, db *sql.DB, epoch string, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO restore_reads(epoch, read_at) VALUES(?, ?) ON CONFLICT DO NOTHING`, epoch, now.Unix())
	return err
}

// RestoreRead reports whether RecordRestoreRead ran for epoch.
func RestoreRead(ctx context.Context, db *sql.DB, epoch string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM restore_reads WHERE epoch = ?`, epoch).Scan(&n)
	return n > 0, err
}

// PurgeCoverage maps every org-owned (O) and derived (D) store of the design's Store
// inventory (tableClass and fileClass in inventory_test.go) to the part of the purge's
// final check that proves it holds nothing of the snapshot (api purgeChecks). Per-user and
// per-mailbox databases and files are "directories": their directory is gone (on
// PostgreSQL also their schemas, "schemas"). "kept" is subject_orgs: opaque IDs that stop a
// subject ever joining another org.
var PurgeCoverage = map[string]string{
	"state.agent_tokens": "root-state", "state.scheduled_sends": "root-state", "state.send_ledger": "root-state",
	"state.sessions": "root-state", "state.session_revocations": "root-state", "state.auth_challenges": "root-state",
	"state.import_jobs": "root-state",
	"state.processed":   "directories", "state.decisions": "directories", "state.notifications": "directories",
	"state.native_devices": "directories", "state.pull_notifications": "directories", "state.deferrals": "directories",
	"state.sorter_predictions": "directories", "state.sorter_corrections": "directories", "state.snoozes": "directories",
	"state.ooo_messages": "directories", "state.ooo_senders": "directories",
	"mailbox.identity": "directories", "mailbox.namespace": "directories", "mailbox.reference_generation": "directories",
	"mailbox.folders": "directories", "mailbox.labels": "directories", "mailbox.messages": "directories",
	"mailbox.incoming_replacements": "directories", "mailbox.imported": "directories", "mailbox.receipts": "directories",
	"mailbox.usage": "directories", "mailbox.changes": "directories", "mailbox.outbox": "directories",
	"mailbox.outbox_deliveries": "directories", "mailbox.outbox_independent": "directories",
	"ingress.routes": "ingress", "ingress.bindings": "ingress",
	"native-authority.directory": "ledger", "native-authority.directory_primary_emails": "ledger",
	"native-authority.accounts": "ledger", "native-authority.mailboxes": "ledger", "native-authority.addresses": "ledger",
	"native-authority.address_history": "ledger", "native-authority.routes_pending": "ledger",
	"tenancy.user_orgs": "user_orgs", "tenancy.subject_orgs": "kept",
	"users.json": "users.json", "quota-": "directories", "tuning.md": "directories",
	"carddav-auth.json": "directories", "carddav-client.json": "directories", "contacts.json": "directories",
	"groups.json": "directories", "rules.json": "directories", "send_as_aliases.json": "directories",
	"pgp-discovery.json": "directories", "pgp-discovery-suppressions.json": "directories",
	"kycalendar-invitations.json": "directories", "kycalendar-flags.json": "directories",
	"incoming-encryption.json": "directories", "mailcache.json": "directories", "native-mailbox.json": "directories",
	"outbox-idle": "directories", "mailbox.db": "directories", "pickup": "pickup",
	// An upload lives only while its job runs; the purge cancels the job, whose own exit
	// removes the file, and deletes the import_jobs row.
	"imports": "root-state",
}
