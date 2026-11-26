package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

const orgOne = "1a2b3c4d-0000-4000-8000-00000000000a"

func openCache(t *testing.T) *Gate {
	t.Helper()
	db, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Gate{DB: db, StaffOrg: staffA}
}

func apply(t *testing.T, g *Gate, status string, rev int64, enabled bool, quotas string, at time.Time) error {
	t.Helper()
	return ApplyOrg(context.Background(), g.DB, suitedir.Org{ID: orgOne, Status: status, Revision: rev, Enabled: enabled, Quotas: json.RawMessage(quotas)}, at)
}

func lookup1(t *testing.T, g *Gate) Org {
	t.Helper()
	o, err := LookupOrg(context.Background(), g.DB, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// The revision fence, deleted as terminal, the stale bound at its edges, the staff
// exemption and the nil gate.
func TestOrgCache(t *testing.T) {
	ctx := context.Background()
	g := openCache(t)
	now := time.Now()
	if c, err := OrgCursor(ctx, g.DB); err != nil || c != 0 {
		t.Fatalf("empty cursor %d %v", c, err)
	}
	if err := g.Serve(ctx, orgOne); !errors.Is(err, ErrNotServed) {
		t.Fatalf("unknown org: %v", err)
	}
	if err := apply(t, g, "active", 5, true, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if err := g.Serve(ctx, orgOne); err != nil {
		t.Fatal(err)
	}
	if c, _ := OrgCursor(ctx, g.DB); c != 4 {
		t.Fatalf("cursor %d, want 4", c)
	}
	// A lower revision changes nothing; an equal one applies the same state.
	if err := apply(t, g, "suspended", 4, true, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); o.Status != "active" || o.Revision != 5 {
		t.Fatalf("stale revision applied: %+v", o)
	}
	if err := apply(t, g, "suspended", 6, true, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if err := g.Serve(ctx, orgOne); !errors.Is(err, ErrNotServed) {
		t.Fatalf("suspended: %v", err)
	}
	if err := apply(t, g, "deleted", 7, true, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if err := apply(t, g, "active", 8, true, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); o.Status != "deleted" || o.Receiving() != ReceiveUnknown {
		t.Fatalf("deleted is not terminal: %+v", o)
	}

	for _, tc := range []struct {
		age   time.Duration
		fresh bool
	}{{0, true}, {StaleAfter, true}, {StaleAfter + time.Second, false}, {-time.Minute, false}} {
		o := Org{Known: true, Status: "active", Enabled: true, PolledAt: now.Add(-tc.age)}
		if o.Fresh(now) != tc.fresh || o.Served(now) != tc.fresh || o.Receiving() != ReceiveAccept {
			t.Errorf("age %v: fresh %v served %v receiving %v", tc.age, o.Fresh(now), o.Served(now), o.Receiving())
		}
	}

	// Only staleness on a served org is ErrStale; it is still ErrNotServed.
	if _, err := g.DB.Exec(`DELETE FROM orgs`); err != nil {
		t.Fatal(err)
	}
	if err := apply(t, g, "active", 1, true, `{}`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := g.Serve(ctx, orgOne); !errors.Is(err, ErrStale) || !errors.Is(err, ErrNotServed) {
		t.Fatalf("stale active org: %v", err)
	}
	if err := apply(t, g, "suspended", 2, true, `{}`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := g.Serve(ctx, orgOne); errors.Is(err, ErrStale) || !errors.Is(err, ErrNotServed) {
		t.Fatalf("stale suspended org: %v", err)
	}

	// MarkPolled stamps every row; staff are never gated; a nil gate serves nobody.
	if err := MarkPolled(ctx, g.DB, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); o.PolledAt.Unix() != now.Add(time.Hour).Unix() {
		t.Fatalf("poll stamp %v", o.PolledAt)
	}
	if err := g.Serve(ctx, staffA); err != nil {
		t.Fatalf("staff org gated: %v", err)
	}
	var none *Gate
	for _, gate := range []*Gate{none, {}, {DB: g.DB}} {
		if gate.Serve(ctx, staffA) == nil || gate.Serve(ctx, "") == nil {
			t.Fatalf("gate %+v served without the staff org or an org", gate)
		}
	}
}

// Malformed entries are refused before they are written.
func TestApplyOrgRefusesMalformedEntries(t *testing.T) {
	g := openCache(t)
	for name, o := range map[string]suitedir.Org{
		"empty id":        {Status: "active", Revision: 1, Quotas: json.RawMessage(`{}`)},
		"upper-case id":   {ID: "1A2B3C4D-0000-4000-8000-00000000000A", Status: "active", Revision: 1},
		"unknown status":  {ID: orgOne, Status: "paused", Revision: 1},
		"null quotas":     {ID: orgOne, Status: "active", Revision: 1, Quotas: json.RawMessage(`null`)},
		"array quotas":    {ID: orgOne, Status: "active", Revision: 1, Quotas: json.RawMessage(`[]`)},
		"negative rev":    {ID: orgOne, Status: "active", Revision: -1},
		"oversize quotas": {ID: orgOne, Status: "active", Revision: 1, Quotas: json.RawMessage(`{"x":"` + string(make([]byte, 5000)) + `"}`)},
	} {
		if err := ApplyOrg(context.Background(), g.DB, o, time.Now()); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	var n int
	if err := g.DB.QueryRow(`SELECT COUNT(*) FROM orgs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows %d %v", n, err)
	}
}

// A missing quota keeps today's default (nil); a bad or negative one reads as 0.
func TestParseQuotas(t *testing.T) {
	n := func(q *int64) any {
		if q == nil {
			return nil
		}
		return *q
	}
	for raw, want := range map[string][5]any{
		`{}`: {nil, nil, nil, nil, nil},
		`{"sendPerHour":10,"recipientsPerDay":100,"mailboxBytes":5,"storageBytes":9,"mailboxes":2,"maskedPerOrg":1}`: {int64(10), int64(100), int64(5), int64(9), int64(2)},
		`{"sendPerHour":-1,"mailboxes":"3","storageBytes":1.5}`:                                                      {int64(0), nil, nil, int64(0), int64(0)},
		`not json`: {int64(0), int64(0), int64(0), int64(0), int64(0)},
	} {
		q := parseQuotas(raw)
		got := [5]any{n(q.SendPerHour), n(q.RecipientsPerDay), n(q.MailboxBytes), n(q.StorageBytes), n(q.Mailboxes)}
		if got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

// MH9: an org of another cell is held as ID, cell and revision only. Every lookup misses
// it, as it misses an org never listed; it still moves the poll cursor; a newer answer
// placing it in this deployment's cells caches it again; a restore forgets it.
func TestForeignOrg(t *testing.T) {
	ctx := context.Background()
	g := openCache(t)
	foreign := suitedir.Org{ID: orgOne, Status: suitedir.OrgActive, Revision: 9, Enabled: true, HomeRegion: "eu-central-1"}
	if err := apply(t, g, "active", 5, true, `{}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ApplyForeignOrg(ctx, g.DB, foreign); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); o != (Org{}) {
		t.Fatalf("a foreign org is cached: %+v", o)
	}
	if err := g.Serve(ctx, orgOne); !errors.Is(err, ErrNotServed) || errors.Is(err, ErrStale) {
		t.Fatalf("foreign org served: %v", err)
	}
	if known, err := KnownOrg(ctx, g.DB, orgOne); err != nil || known {
		t.Fatalf("staff can select a foreign org: %v %v", known, err)
	}
	if ids, err := ReportableOrgs(ctx, g.DB); err != nil || len(ids) != 0 {
		t.Fatalf("a foreign org is reported for: %v %v", ids, err)
	}
	if is, err := Foreign(ctx, g.DB, orgOne); err != nil || !is {
		t.Fatalf("Foreign = %v %v", is, err)
	}
	if c, _ := OrgCursor(ctx, g.DB); c != 8 {
		t.Fatalf("cursor %d, want 8", c)
	}
	// A stale own-cell answer changes nothing; a newer one caches it again.
	if err := apply(t, g, "active", 8, true, `{}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if lookup1(t, g).Known {
		t.Fatal("a stale answer cached a foreign org")
	}
	own := foreign
	own.Revision, own.HomeRegion = 10, "us-east-1"
	if err := ApplyOrg(ctx, g.DB, own, time.Now()); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); !o.Known || o.HomeRegion != "us-east-1" {
		t.Fatalf("own org: %+v", o)
	}
	if is, _ := Foreign(ctx, g.DB, orgOne); is {
		t.Fatal("still foreign")
	}
	// A deleted tombstone is never dropped.
	own.Revision, own.Status = 11, suitedir.OrgDeleted
	if err := ApplyOrg(ctx, g.DB, own, time.Now()); err != nil {
		t.Fatal(err)
	}
	foreign.Revision = 12
	if err := ApplyForeignOrg(ctx, g.DB, foreign); err != nil {
		t.Fatal(err)
	}
	if o := lookup1(t, g); o.Status != suitedir.OrgDeleted {
		t.Fatalf("tombstone dropped: %+v", o)
	}
	if err := ForgetRestored(ctx, g.DB); err != nil {
		t.Fatal(err)
	}
	if is, _ := Foreign(ctx, g.DB, orgOne); is {
		t.Fatal("a restore kept a foreign org")
	}
	if err := ApplyForeignOrg(ctx, g.DB, suitedir.Org{ID: "not-an-org", Status: suitedir.OrgActive, HomeRegion: "x-1"}); err == nil {
		t.Fatal("malformed foreign org recorded")
	}
}
