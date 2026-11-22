package tenancy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// The entitlement cache (K8, K "Lifecycle"). One elected poller writes it from
// KyIdentity's /api/suite/orgs; every replica reads it.

// StaleAfter is K's stale-cache bound: once the last good poll is older, sign-in and
// sending are refused (K open question 5, the access-token lifetime).
const StaleAfter = 15 * time.Minute

// ErrNotServed refuses sign-in, a session or a send for an org that is not active, is
// disabled for KyPost, is missing from the cache, or whose cache is stale. It names no org.
var ErrNotServed = errors.New("this account's organisation is not served")

// Quotas are the quotas_json keys KyPost enforces (K "Abuse limits per org"). A missing
// key is nil: today's default applies. The masked-address keys are PR 7's (M D7).
type Quotas struct {
	SendPerHour      *int64 `json:"sendPerHour"`
	RecipientsPerDay *int64 `json:"recipientsPerDay"`
	MailboxBytes     *int64 `json:"mailboxBytes"`
	StorageBytes     *int64 `json:"storageBytes"`
	Mailboxes        *int64 `json:"mailboxes"`
}

// Org is one cached org.
type Org struct {
	Known    bool
	Status   string
	Revision int64
	Enabled  bool
	Quotas   Quotas
	PolledAt time.Time
}

// Fresh reports whether the last good poll that confirmed o is within StaleAfter. A
// poll stamp in the future is not fresh.
func (o Org) Fresh(now time.Time) bool {
	age := now.Sub(o.PolledAt)
	return o.Known && age >= 0 && age <= StaleAfter
}

// Served reports whether o's members may sign in and send: active, enabled for KyPost
// and fresh (K "Org status").
func (o Org) Served(now time.Time) bool {
	return o.Fresh(now) && o.Status == suitedir.OrgActive && o.Enabled
}

// Receiving is what an inbound RCPT for an org's mailbox gets (K "Org status", open
// question 5). It reads the last known status whatever the cache's age.
type Receiving int

const (
	ReceiveAccept    Receiving = iota // active and enabled
	ReceiveTemporary                  // suspended, disabled or not in the cache: 451, exit 12
	ReceiveUnknown                    // deletion_scheduled or deleted: 550 like an unknown address
)

func (o Org) Receiving() Receiving {
	switch {
	case !o.Known:
		return ReceiveTemporary
	case o.Status == suitedir.OrgDeletionScheduled || o.Status == suitedir.OrgDeleted:
		return ReceiveUnknown
	case o.Status == suitedir.OrgActive && o.Enabled:
		return ReceiveAccept
	}
	return ReceiveTemporary
}

// LookupOrg reads org's cache row; a missing row is Org{Known: false}.
func LookupOrg(ctx context.Context, db *sql.DB, org string) (Org, error) {
	if db == nil {
		return Org{}, ErrOrgMismatch
	}
	var (
		o       Org
		enabled int64
		quotas  string
		polled  int64
	)
	err := db.QueryRowContext(ctx, `SELECT status, revision, enabled, quotas_json, polled_at FROM orgs WHERE id = ?`, org).Scan(&o.Status, &o.Revision, &enabled, &quotas, &polled)
	if errors.Is(err, sql.ErrNoRows) {
		return Org{}, nil
	}
	if err != nil {
		return Org{}, err
	}
	o.Known, o.Enabled, o.PolledAt, o.Quotas = true, enabled == 1, time.Unix(polled, 0), parseQuotas(quotas)
	return o, nil
}

// parseQuotas reads quotas_json. A known key that is not a whole number, or is
// negative, is read as 0, which refuses new mail and creations and never existing data
// (K "Quotas"); a body that does not decode zeroes every key.
func parseQuotas(raw string) Quotas {
	var m map[string]json.RawMessage
	zero := int64(0)
	var q Quotas
	fields := map[string]**int64{"sendPerHour": &q.SendPerHour, "recipientsPerDay": &q.RecipientsPerDay,
		"mailboxBytes": &q.MailboxBytes, "storageBytes": &q.StorageBytes, "mailboxes": &q.Mailboxes}
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		for _, f := range fields {
			*f = &zero
		}
		return q
	}
	for key, f := range fields {
		v, ok := m[key]
		if !ok {
			continue
		}
		var n int64
		if json.Unmarshal(v, &n) != nil || n < 0 {
			n = 0
		}
		*f = &n
	}
	return q
}

// OrgCursor is the since value for the next poll: one below the highest cached
// revision, so orgs sharing it that a cut-short page left out are listed again (an
// equal revision applies as the same state), or 0 with an empty cache.
func OrgCursor(ctx context.Context, db *sql.DB) (int64, error) {
	var top sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(revision) FROM orgs`).Scan(&top); err != nil {
		return 0, err
	}
	return max(top.Int64-1, 0), nil
}

// ApplyOrg writes one listed org in its own statement (K "one transaction per org"). A
// lower revision than the cached one is stale and changes nothing, and deleted is
// terminal (T "Account holder guard"). An entry that is not canonical is refused.
func ApplyOrg(ctx context.Context, db *sql.DB, o suitedir.Org, now time.Time) error {
	if !config.ValidOrgID(o.ID) || o.Revision < 0 || len(o.Quotas) > 4<<10 {
		return errors.New("tenancy: malformed org entry")
	}
	switch o.Status {
	case suitedir.OrgActive, suitedir.OrgSuspended, suitedir.OrgDeletionScheduled, suitedir.OrgDeleted:
	default:
		return errors.New("tenancy: malformed org entry")
	}
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	quotas := string(o.Quotas)
	if quotas == "" {
		quotas = "{}"
	}
	_, err := db.ExecContext(ctx, `INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET status = excluded.status, revision = excluded.revision, enabled = excluded.enabled,
 quotas_json = excluded.quotas_json, polled_at = excluded.polled_at
WHERE excluded.revision >= orgs.revision AND orgs.status <> 'deleted'`, o.ID, o.Status, o.Revision, enabled, quotas, now.Unix())
	return err
}

// MarkPolled stamps every cached org with a good poll's time: KyIdentity answered for
// all of them, changed or not.
// ponytail: one UPDATE over every row each minute; a separate poll-stamp row is the
// upgrade if the org count makes this slow.
func MarkPolled(ctx context.Context, db *sql.DB, now time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE orgs SET polled_at = ? WHERE polled_at < ?`, now.Unix(), now.Unix())
	return err
}

// OrgUsers lists the accounts bound to org in user_orgs.
func OrgUsers(ctx context.Context, db *sql.DB, org string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM user_orgs WHERE org_id = ? ORDER BY user_id`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Gate answers whether an org is served, for a process that holds the cache. The staff
// org is never gated (K8); a nil Gate serves nobody, so an unwired caller fails closed.
type Gate struct {
	DB       *sql.DB
	StaffOrg string
	Now      func() time.Time
}

// Serve is nil when org's members may sign in and send now, ErrNotServed when not, and
// the read error when the cache cannot be read.
func (g *Gate) Serve(ctx context.Context, org string) error {
	if g == nil || g.DB == nil || org == "" {
		return ErrNotServed
	}
	if g.StaffOrg != "" && org == g.StaffOrg {
		return nil
	}
	o, err := LookupOrg(ctx, g.DB, org)
	if err != nil {
		return err
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	if !o.Served(now()) {
		return ErrNotServed
	}
	return nil
}
