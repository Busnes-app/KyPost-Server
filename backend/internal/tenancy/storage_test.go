package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
 "sync"
 "sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

const orgTwo = "1a2b3c4d-0000-4000-8000-00000000000b"

// Sequence numbers come from the shared database, and only the answer to the latest
// report is recorded. Reservations and expiry are exercised against the real row below.
func TestStorageGrant(t *testing.T) {
	ctx := context.Background()
	db := openCache(t).DB
	now := time.Now()

	if got, err := StorageGrant(ctx, db, orgOne); err != nil || got != (Grant{}) {
		t.Fatalf("an org never reported has a grant: %+v %v", got, err)
	}
	s1, _, err := NextStorageSeq(ctx, db, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := NextStorageSeq(ctx, db, orgOne)
	if err != nil || s2 != s1+1 {
		t.Fatalf("seq %d then %d %v", s1, s2, err)
	}
	if s, _, err := NextStorageSeq(ctx, db, orgTwo); err != nil || s != 1 {
		t.Fatalf("another org's first seq %d %v", s, err)
	}
	want := Grant{Used: 10, Reported: 10, Ceiling: 50, Expires: time.Unix(now.Add(time.Minute).Unix(), 0)}
	// The answer to the older report arrives late: it changes nothing.
	if err := SetGrant(ctx, db, orgOne, s1, Grant{Used: 99, Ceiling: 1 << 40, Expires: want.Expires}); err != nil {
		t.Fatal(err)
	}
	if got, _ := StorageGrant(ctx, db, orgOne); got.Ceiling != 0 {
		t.Fatalf("a stale answer was recorded: %+v", got)
	}
	if err := SetGrant(ctx, db, orgOne, s2, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := StorageGrant(ctx, db, orgOne); got != want {
		t.Fatalf("grant %+v, want %+v", got, want)
	}
	if got, _ := StorageGrant(ctx, db, orgTwo); got.Ceiling != 0 {
		t.Fatalf("org two holds org one's grant: %+v", got)
	}
	if _, last, err := NextStorageSeq(ctx, db, orgOne); err != nil || last != want {
		t.Fatalf("the next report does not see the last grant: %+v %v", last, err)
	}
}

type storageIdP struct {
	*httptest.Server
	answer func(reports []StorageReport) (int, any)
	got    []StorageReport
	hits   int
}

func newStorageIdP(t *testing.T) *storageIdP {
	idp := &storageIdP{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp.hits++
		id, secret, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/api/suite/storage" || !ok || id != "kypost" || secret != "suite-secret" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		var body struct {
			Reports []StorageReport `json:"reports"`
		}
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &body) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		idp.got = body.Reports
		status, out := idp.answer(body.Reports)
		w.WriteHeader(status)
		if s, ok := out.(string); ok {
			_, _ = io.WriteString(w, s)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(idp.Close)
	return idp
}

func grants(reports []StorageReport) []StorageResult {
	var out []StorageResult
	for _, r := range reports {
		out = append(out, StorageResult{OrgID: r.OrgID, Result: "ok", Seq: r.Seq, CeilingBytes: r.UsedBytes + r.WantBytes, TTLSeconds: 900})
	}
	return out
}

// ReportStorage sends the reports with the client's credentials and accepts only an
// answer that gives each report exactly one well-formed result; anything else is refused
// whole. It refuses an insecure issuer, wrong credentials, a redirect and an oversize body.
func TestReportStorage(t *testing.T) {
	ctx := context.Background()
	idp := newStorageIdP(t)
	dir := suitedir.Client{Issuer: idp.URL, ClientID: "kypost", ClientSecret: "suite-secret"}
	reports := []StorageReport{{OrgID: orgOne, UsedBytes: 5, Seq: 3, WantBytes: 10}, {OrgID: orgTwo, UsedBytes: 0, Seq: 1, WantBytes: 10}}

	idp.answer = func(r []StorageReport) (int, any) {
		// Out of order: matched by org, not position.
		g := grants(r)
		g[0], g[1] = g[1], g[0]
		return 200, map[string]any{"results": g}
	}
	got, err := ReportStorage(ctx, dir, reports)
	if err != nil || len(got) != 2 || got[0].OrgID != orgOne || got[0].CeilingBytes != 15 || got[1].OrgID != orgTwo {
		t.Fatalf("results %+v %v", got, err)
	}
	if len(idp.got) != 2 || idp.got[0] != reports[0] {
		t.Fatalf("KyIdentity got %+v", idp.got)
	}

	malformed := map[string]func(r []StorageReport) []StorageResult{
		"missing org": func(r []StorageReport) []StorageResult { return grants(r)[:1] },
		"another org": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[1].OrgID = "1a2b3c4d-0000-4000-8000-00000000000c"
			return g
		},
		"repeated org": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[1] = g[0]
			return g
		},
		"grant for another seq": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].Seq++
			return g
		},
		"unknown result": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].Result = "granted"
			return g
		},
		"negative ceiling": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].CeilingBytes = -1
			return g
		},
		"oversize ceiling": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].CeilingBytes = 1<<53 + 1
			return g
		},
		"negative pool": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].PoolBytes = -1
			return g
		},
		"oversize org usage": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].OrgUsedBytes = 1<<53 + 1
			return g
		},
		"zero over_since": func(r []StorageReport) []StorageResult {
			g := grants(r)
			zero := int64(0)
			g[0].OverSince = &zero
			return g
		},
		"negative ttl": func(r []StorageReport) []StorageResult {
			g := grants(r)
			g[0].TTLSeconds = -1
			return g
		},
	}
	for name, results := range malformed {
		idp.answer = func(r []StorageReport) (int, any) { return 200, map[string]any{"results": results(r)} }
		if _, err := ReportStorage(ctx, dir, reports); !errors.Is(err, ErrStorageAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A stale report and an unknown org are answers, not errors.
	idp.answer = func(r []StorageReport) (int, any) {
		g := grants(r)
		g[0].Result, g[0].Seq, g[1].Result, g[1].Seq = "stale", 9, "unknown_org", 0
		return 200, map[string]any{"results": g}
	}
	if got, err := ReportStorage(ctx, dir, reports); err != nil || got[0].Result != "stale" || got[1].Result != "unknown_org" {
		t.Fatalf("stale and unknown: %+v %v", got, err)
	}

	for name, answer := range map[string]func(r []StorageReport) (int, any){
		"not JSON": func([]StorageReport) (int, any) { return 200, "{" },
		"oversize": func([]StorageReport) (int, any) {
			return 200, `{"results":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`
		},
		"server error": func([]StorageReport) (int, any) { return 500, "down" },
		"refused":      func([]StorageReport) (int, any) { return 400, `{"error":"invalid_request"}` },
	} {
		idp.answer = answer
		if _, err := ReportStorage(ctx, dir, reports); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	idp.answer = func(r []StorageReport) (int, any) { return 200, map[string]any{"results": grants(r)} }
	wrong := dir
	wrong.ClientSecret = "other-secret"
	if _, err := ReportStorage(ctx, wrong, reports); !errors.Is(err, suitedir.ErrUnauthorized) {
		t.Fatalf("wrong credentials: %v", err)
	}
	if _, err := ReportStorage(ctx, suitedir.Client{Issuer: "http://identity.example.test", ClientID: "kypost", ClientSecret: "suite-secret"}, reports); !errors.Is(err, suitedir.ErrInsecureIssuer) {
		t.Fatalf("plain HTTP off loopback: %v", err)
	}
	redirect := httptest.NewServer(http.RedirectHandler(idp.URL+"/api/suite/storage", http.StatusTemporaryRedirect))
	defer redirect.Close()
	hits := idp.hits
	if _, err := ReportStorage(ctx, suitedir.Client{Issuer: redirect.URL, ClientID: "kypost", ClientSecret: "suite-secret"}, reports); err == nil || idp.hits != hits {
		t.Fatalf("followed a redirect: %v", err)
	}
	for _, n := range []int{0, MaxStorageReports + 1} {
		if _, err := ReportStorage(ctx, dir, make([]StorageReport, n)); err == nil {
			t.Errorf("%d reports sent", n)
		}
	}
}

// Incoming mail over the pool (owner decision of 2026-11-23): within the grant it is
// accepted; past it, 7 days' grace and up to max(10 %, 2 GB) over the pool, whichever ends
// first; then 452 for 3 days from the end of grace; then 552. The overrun starts at the
// earlier of KyIdentity's date and KyPost's own first sighting. No pool means 452 at once.
func TestPoolMail(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const gb = 1_000_000_000
	day := 24 * time.Hour
	over := Grant{Used: 18 * gb, Reported: 18 * gb, Ceiling: 18 * gb, Expires: now.Add(time.Minute), PoolBytes: 20 * gb, OrgUsed: 21 * gb}
	at := func(g Grant, f func(*Grant)) Grant { f(&g); return g }
	for _, c := range []struct {
		name            string
		g               Grant
		used, add       int64
		at              time.Time
		want            PoolMail
		first, graceEnd bool
	}{
		{name: "fits the grant", g: at(over, func(g *Grant) { g.Ceiling = 19 * gb }), used: 18 * gb, add: gb, at: now, want: PoolMailAccept},
		{name: "no pool", g: Grant{}, used: 0, add: 1, at: now, want: PoolMailTemporary},
		{name: "first sighting starts grace", g: over, used: 18 * gb, add: gb, at: now, want: PoolMailAccept, first: true},
		{name: "KyIdentity's date, day 6", g: at(over, func(g *Grant) { g.OverSince = now.Add(-6 * day) }), used: 18 * gb, add: gb, at: now, want: PoolMailAccept},
		// The overage is 2 GB: 3 GB over the pool already plus 1 byte passes it.
		{name: "overage spent", g: at(over, func(g *Grant) { g.OrgUsed = 23 * gb; g.OverSince = now.Add(-day) }), used: 18 * gb, add: 1, at: now, want: PoolMailTemporary, graceEnd: true},
		{name: "exactly at the overage", g: at(over, func(g *Grant) { g.OrgUsed = 21 * gb; g.OverSince = now.Add(-day) }), used: 18 * gb, add: gb, at: now, want: PoolMailAccept},
		{name: "day 8: grace over", g: at(over, func(g *Grant) { g.OverSince = now.Add(-8 * day) }), used: 18 * gb, add: 1, at: now, want: PoolMailTemporary},
		{name: "day 10: refused", g: at(over, func(g *Grant) { g.OverSince = now.Add(-10 * day) }), used: 18 * gb, add: 1, at: now, want: PoolMailRefuse},
		{name: "3 days after the overage ran out", g: at(over, func(g *Grant) { g.OverSince = now.Add(-4 * day); g.GraceEnd = now.Add(-3 * day) }), used: 18 * gb, add: 1, at: now, want: PoolMailRefuse},
		{name: "KyIdentity's earlier date wins", g: at(over, func(g *Grant) { g.OverSince = now.Add(-10 * day); g.FirstOver = now.Add(-day) }), used: 18 * gb, add: 1, at: now, want: PoolMailRefuse},
		{name: "the earlier date wins", g: at(over, func(g *Grant) { g.OverSince = now.Add(-day); g.FirstOver = now.Add(-10 * day) }), used: 18 * gb, add: 1, at: now, want: PoolMailRefuse},
		{name: "an expired grant falls to the rule", g: at(over, func(g *Grant) { g.Ceiling = 1 << 40; g.Expires = now }), used: 18 * gb, add: gb, at: now, want: PoolMailAccept, first: true},
		{name: "a future date is now", g: at(over, func(g *Grant) { g.OverSince = now.Add(day) }), used: 18 * gb, add: gb, at: now, want: PoolMailAccept, first: true},
	} {
		got, first, end := c.g.Mail(c.used, c.add, c.at)
		if got != c.want || first.IsZero() == c.first || end.IsZero() == c.graceEnd {
			t.Errorf("%s: %v first %v end %v, want %v first %v end %v", c.name, got, first, end, c.want, c.first, c.graceEnd)
		}
	}
	if PoolOverage(20*gb) != 2*gb || PoolOverage(3000*gb) != 300*gb || PoolOverage(gb) != 2*gb {
		t.Fatal("overage is not max(10 %, 2 GB)")
	}
}

// The overrun dates are written once, and an answer within the pool clears them.
func TestOverrunMarks(t *testing.T) {
	ctx := context.Background()
	db := openCache(t).DB
	seq, _, err := NextStorageSeq(ctx, db, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	first, later := time.Unix(1_800_000_000, 0), time.Unix(1_800_000_600, 0)
	if err := MarkOverrun(ctx, db, orgOne, first, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := MarkOverrun(ctx, db, orgOne, later, later); err != nil {
		t.Fatal(err)
	}
	g, err := StorageGrant(ctx, db, orgOne)
	if err != nil || !g.FirstOver.Equal(first) || !g.GraceEnd.Equal(later) {
		t.Fatalf("marks %+v %v", g, err)
	}
	// Still over: the marks stay.
	if err := SetGrant(ctx, db, orgOne, seq, Grant{PoolBytes: 10, OrgUsed: 11}); err != nil {
		t.Fatal(err)
	}
	if g, _ := StorageGrant(ctx, db, orgOne); g.FirstOver.IsZero() || g.PoolBytes != 10 || g.OrgUsed != 11 {
		t.Fatalf("an answer over the pool cleared the overrun: %+v", g)
	}
	if err := SetGrant(ctx, db, orgOne, seq, Grant{PoolBytes: 10, OrgUsed: 9, OverSince: first}); err != nil {
		t.Fatal(err)
	}
	if g, _ := StorageGrant(ctx, db, orgOne); g.FirstOver.IsZero() || !g.OverSince.Equal(first) {
		t.Fatalf("KyIdentity still says over, but the overrun was cleared: %+v", g)
	}
	if err := SetGrant(ctx, db, orgOne, seq, Grant{PoolBytes: 10, OrgUsed: 10}); err != nil {
		t.Fatal(err)
	}
	if g, _ := StorageGrant(ctx, db, orgOne); !g.FirstOver.IsZero() || !g.GraceEnd.IsZero() || !g.OverSince.IsZero() {
		t.Fatalf("back within the pool, the overrun stays: %+v", g)
	}
}

// Reservations serialize per org, and a report answer never erases growth admitted
// while it measured usage or waited for KyIdentity. Failed writes stay conservative.
func TestStorageReservationsSurviveReport(t *testing.T) {
	ctx, db := context.Background(), openCache(t).DB
	seq, _, err := NextStorageSeq(ctx, db, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Minute)
	if err := SetGrant(ctx, db, orgOne, seq, Grant{Ceiling: 100, Expires: expiry}); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ok, err := ReserveStorage(ctx, db, orgOne, 10, time.Now())
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("accepted %d writes into 100 bytes", accepted.Load())
	}
	snapshot, err := StorageGrant(ctx, db, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	seq, _, err = NextStorageSeq(ctx, db, orgOne)
	if err != nil {
		t.Fatal(err)
	}
	// A deletion is seen by the scan; more writes can spend a renewed old grant
	// while its HTTP answer is pending.
	if _, err := db.ExecContext(ctx, `UPDATE org_storage SET ceiling_bytes = 200 WHERE org_id = ?`, orgOne); err != nil {
		t.Fatal(err)
	}
	if ok, err := ReserveStorage(ctx, db, orgOne, 30, time.Now()); err != nil || !ok {
		t.Fatalf("reserve while reporting: %v %v", ok, err)
	}
	if err := SetGrant(ctx, db, orgOne, seq, Grant{Used: 40, Admitted: snapshot.Admitted, Ceiling: 80, Expires: expiry}); err != nil {
		t.Fatal(err)
	}
	g, err := StorageGrant(ctx, db, orgOne)
	if err != nil || g.Used != 70 || g.Admitted != 130 {
		t.Fatalf("report erased reservations: %+v %v", g, err)
	}
	if ok, err := ReserveStorage(ctx, db, orgOne, 11, time.Now()); err != nil || ok {
		t.Fatalf("spent more than remaining headroom: %v %v", ok, err)
	}
	if ok, err := ReserveStorage(ctx, db, orgTwo, 1, time.Now()); err != nil || ok {
		t.Fatalf("missing org borrowed a grant: %v %v", ok, err)
	}
	if ok, err := ReserveStorage(ctx, db, orgOne, 1, expiry); err != nil || ok {
		t.Fatalf("expired grant: %v %v", ok, err)
	}
}
