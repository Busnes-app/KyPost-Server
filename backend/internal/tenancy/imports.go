package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

// Receiving import in multi mode (RX R8): one obligation per bound mailbox, done when the
// mailbox holds the receipt or its binding is in retired_bindings, and one worker per
// mailbox at a time under an import_leases row. Every write is one statement, which the
// store's single writer serializes, so two workers cannot both take a free lease.

// ImportLease is how long a taken or renewed lease lasts.
const ImportLease = 60 * time.Second

// TakeImportLease takes mailbox's lease for holder until now+ImportLease. A free or
// expired lease is taken; holder's own live lease is renewed. It reports false, and
// changes nothing, while another holder's lease is live.
func TakeImportLease(ctx context.Context, db *sql.DB, mailbox, holder string, now time.Time) (bool, error) {
	if db == nil || mailbox == "" || holder == "" {
		return false, ErrOrgMismatch
	}
	res, err := db.ExecContext(ctx, `INSERT INTO import_leases(mailbox, holder, lease_until) VALUES(?, ?, ?)
 ON CONFLICT(mailbox) DO UPDATE SET holder = excluded.holder, lease_until = excluded.lease_until
 WHERE import_leases.lease_until <= ? OR import_leases.holder = excluded.holder`,
		mailbox, holder, now.Add(ImportLease).Unix(), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseImportLease gives up holder's lease on mailbox. Another holder's is kept.
func ReleaseImportLease(ctx context.Context, db *sql.DB, mailbox, holder string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM import_leases WHERE mailbox = ? AND holder = ?`, mailbox, holder)
	return err
}

// RecordRetired records that mailbox's binding of delivery (gateway, id) can never import.
// A repeat is a no-op.
func RecordRetired(ctx context.Context, db *sql.DB, gateway, id, mailbox string, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO retired_bindings(gateway, id, mailbox, at) VALUES(?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		gateway, id, mailbox, now.Unix())
	return err
}

// Retired reports whether mailbox's binding of (gateway, id) is recorded retired.
func Retired(ctx context.Context, db *sql.DB, gateway, id, mailbox string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM retired_bindings WHERE gateway = ? AND id = ? AND mailbox = ?`, gateway, id, mailbox).Scan(&n)
	return n > 0, err
}

// Import is what receiving import does with a mailbox's mail, from its org's last known
// status (RX "Org status"): K keeps a suspended or deletion_scheduled org's data, so its
// accepted mail imports; a deleted org's waits for the purge; an org missing from the
// cache waits for a good poll.
type Import int

const (
	ImportWait  Import = iota // not in the cache
	ImportNow                 // active, suspended or deletion_scheduled, enabled or not
	ImportNever               // deleted: the purge drops the obligation
)

func (o Org) Import() Import {
	switch {
	case !o.Known:
		return ImportWait
	case o.Status == suitedir.OrgDeleted:
		return ImportNever
	}
	return ImportNow
}

// MailboxImport is Import for the mailbox owned by (issuer, subject). A subject with no
// recorded org waits.
func MailboxImport(ctx context.Context, db *sql.DB, issuer, subject string) (Import, error) {
	org, err := SubjectOrg(ctx, db, issuer, subject)
	if err != nil || org == "" {
		return ImportWait, err
	}
	o, err := LookupOrg(ctx, db, org)
	if err != nil {
		return ImportWait, err
	}
	return o.Import(), nil
}

// ErrImportDeleted is a mailbox import refused because the owner's org is deleted.
var ErrImportDeleted = errors.New("the mailbox's organisation is deleted; the purge removes its mail")
