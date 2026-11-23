package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

const orgTwo = "1a2b3c4d-0000-4000-8000-00000000000b"

// The grant fits at exactly its ceiling and not one byte past it, never once expired, and
// a zero grant fits nothing. Sequence numbers come from the database and only the answer
// to the latest one is recorded.
func TestStorageGrant(t *testing.T) {
	ctx := context.Background()
	db := openCache(t).DB
	now := time.Now()
	g := Grant{Ceiling: 100, Expires: now.Add(time.Minute)}
	for _, c := range []struct {
		used, add int64
		at        time.Time
		fits      bool
	}{
		{60, 40, now, true}, {60, 41, now, false}, {100, 0, now, true}, {101, 0, now, false},
		{0, 1, now.Add(time.Minute), false}, {-1, 1, now, false}, {0, -1, now, false},
		{0, 1 << 62, now, false},
	} {
		if g.Fits(c.used, c.add, c.at) != c.fits {
			t.Errorf("Fits(%d, %d) = %v", c.used, c.add, !c.fits)
		}
	}
	if (Grant{}).Fits(0, 1, now) {
		t.Fatal("no grant fits a write")
	}

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
	want := Grant{Used: 10, Ceiling: 50, Expires: time.Unix(now.Add(time.Minute).Unix(), 0)}
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
