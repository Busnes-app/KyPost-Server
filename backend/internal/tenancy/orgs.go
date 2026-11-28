package tenancy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// ErrStale is ErrNotServed for an org whose last known state is served but whose cache
// is past StaleAfter: an upstream outage, which a credential check answers as "try again
// later" rather than as a dead credential.
var ErrStale = fmt.Errorf("%w: the entitlement cache is stale", ErrNotServed)

// Quotas are the quotas_json keys KyPost enforces (K "Abuse limits per org"). A missing
// key is nil: today's default applies. The masked-address keys (M D7) are the hosted launch gate's.
type Quotas struct {
	SendPerHour      *int64 `json:"sendPerHour"`
	RecipientsPerDay *int64 `json:"recipientsPerDay"`
	MailboxBytes     *int64 `json:"mailboxBytes"`
	StorageBytes     *int64 `json:"storageBytes"`
	Mailboxes        *int64 `json:"mailboxes"`
}

// Org is one cached org. HomeRegion is its cell (KyIdentity tenancy spec, Cells, C1), one of
// this deployment's; MH11's object keys start with it ("<app>/cells/<home_region>/").
type Org struct {
	Known      bool
	Status     string
	Revision   int64
	Enabled    bool
	Quotas     Quotas
	PolledAt   time.Time
	HomeRegion string
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
	err := db.QueryRowContext(ctx, `SELECT status, revision, enabled, quotas_json, polled_at, home_region FROM orgs WHERE id = ?`, org).Scan(&o.Status, &o.Revision, &enabled, &quotas, &polled, &o.HomeRegion)
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
// revision, other cells' orgs included, so orgs sharing it that a cut-short page left out
// are listed again (an equal revision applies as the same state), or 0 with an empty cache.
func OrgCursor(ctx context.Context, db *sql.DB) (int64, error) {
	var top sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(revision) FROM (SELECT revision FROM orgs UNION ALL SELECT revision FROM foreign_orgs) AS listed`).Scan(&top); err != nil {
		return 0, err
	}
	return max(top.Int64-1, 0), nil
}

// ValidOrg refuses a listed org that ApplyOrg would not write: an ID that is not
// canonical, a negative revision, an unknown status, or quotas over 4 KiB or not a JSON
// object (null included; an absent field is {}).
func ValidOrg(o suitedir.Org) error {
	var quotas map[string]json.RawMessage
	switch {
	case !config.ValidOrgID(o.ID) || o.Revision < 0 || len(o.Quotas) > 4<<10:
	case len(o.Quotas) > 0 && (json.Unmarshal(o.Quotas, &quotas) != nil || quotas == nil):
	case o.Status == suitedir.OrgActive, o.Status == suitedir.OrgSuspended, o.Status == suitedir.OrgDeletionScheduled, o.Status == suitedir.OrgDeleted:
		return nil
	}
	return errors.New("tenancy: malformed org entry")
}

// ApplyOrg writes one listed org of this deployment's cells (K "one transaction per org").
// A lower revision than the cached one is stale and changes nothing, and deleted is
// terminal (T "Account holder guard"). An entry that is not canonical is refused.
func ApplyOrg(ctx context.Context, db *sql.DB, o suitedir.Org, now time.Time) error {
	if err := ValidOrg(o); err != nil {
		return err
	}
	// C4: an org never changes cell; if KyIdentity ever says otherwise, the newest answer wins.
	var foreign sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(revision) FROM foreign_orgs WHERE id = ?`, o.ID).Scan(&foreign); err != nil {
		return err
	}
	if foreign.Valid && foreign.Int64 > o.Revision {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM foreign_orgs WHERE id = ?`, o.ID); err != nil {
		return err
	}
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	quotas := string(o.Quotas)
	if quotas == "" {
		quotas = "{}"
	}
	_, err := db.ExecContext(ctx, `INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at, home_region) VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET status = excluded.status, revision = excluded.revision, enabled = excluded.enabled,
 quotas_json = excluded.quotas_json, polled_at = excluded.polled_at, home_region = excluded.home_region
WHERE excluded.revision >= orgs.revision AND orgs.status <> 'deleted'`, o.ID, o.Status, o.Revision, enabled, quotas, now.Unix(), o.HomeRegion)
	return err
}

// ApplyForeignOrg records a listed org of another cell (MH9; KyIdentity tenancy spec, Cells,
// C5) as its ID, cell and revision only, and drops any cached row for it that is not a
// deleted tombstone, so every way in answers it as an org the cache does not hold. A lower
// revision than the recorded one changes nothing.
func ApplyForeignOrg(ctx context.Context, db *sql.DB, o suitedir.Org) error {
	if err := ValidOrg(o); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM orgs WHERE id = ? AND revision <= ? AND status <> 'deleted'`, o.ID, o.Revision); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO foreign_orgs(id, home_region, revision) VALUES(?, ?, ?)
ON CONFLICT(id) DO UPDATE SET home_region = excluded.home_region, revision = excluded.revision
WHERE excluded.revision >= foreign_orgs.revision`, o.ID, o.HomeRegion, o.Revision)
	return err
}

// Foreign reports whether org is recorded as another cell's (ApplyForeignOrg).
func Foreign(ctx context.Context, db *sql.DB, org string) (bool, error) {
	id, err := lookup(ctx, db, `SELECT id FROM foreign_orgs WHERE id = ?`, org)
	return id != "", err
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
	switch {
	case o.Served(now()):
	case o.Known && o.Status == suitedir.OrgActive && o.Enabled:
		return ErrStale
	default:
		return ErrNotServed
	}
	return nil
}

// ErrOrgCell refuses a mailbox or address of an org this deployment cannot place: no org,
// an org not in the cache, or one whose home region is not a served cell (MH9, C5).
var ErrOrgCell = errors.New("tenancy: the org is not in a cell this deployment serves")

// OrgCell is the cell org's data lives in: its cached home region, which must be one of
// cells. The staff org is in every cell, so outside its own region it uses cells[0], the
// first of the sorted KY_CELLS; a deployment's cells are fixed, as orgs never move.
func OrgCell(ctx context.Context, db *sql.DB, org, staffOrg string, cells []string) (string, error) {
	if org == "" || len(cells) == 0 {
		return "", ErrOrgCell
	}
	o, err := LookupOrg(ctx, db, org)
	if err != nil {
		return "", err
	}
	switch {
	case o.Known && slices.Contains(cells, o.HomeRegion):
		return o.HomeRegion, nil
	case org == staffOrg:
		return cells[0], nil
	}
	return "", ErrOrgCell
}
