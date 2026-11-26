package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// cellHost is one in-process KyPost host serving one cell (MH9).
type cellHost struct {
	srv *Server
	idp *ssotest.IdP
	dir suitedir.Client
}

func newCellHost(t *testing.T, cell string, orgs *orgsIdP) cellHost {
	t.Helper()
	srv, idp := nativeSignOnServer(t)
	srv.pairingSecret = testSyncKey
	db := makeMultiTenant(t, srv)
	srv.cells = []string{cell}
	if _, err := db.Exec(`DELETE FROM orgs`); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://cal.example.com")
	srv.SetKyCalendar(kycalendar.New(base, kycalSecret, http.DefaultTransport))
	return cellHost{srv: srv, idp: idp, dir: suitedir.Client{Issuer: orgs.URL, ClientID: "kypost", ClientSecret: "suite-secret"}}
}

func (h cellHost) poll(t *testing.T) {
	t.Helper()
	since, err := tenancy.OrgCursor(context.Background(), h.srv.tenancy)
	if err == nil {
		err = h.srv.pollOrgsFrom(context.Background(), h.dir, since, time.Now())
	}
	if err != nil {
		t.Fatal(err)
	}
}

// writes counts the rows and accounts a refused event must not have written.
func (h cellHost) writes(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.srv.tenancy.QueryRow(`SELECT (SELECT COUNT(*) FROM subject_orgs) + (SELECT COUNT(*) FROM user_orgs)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	all, _ := h.srv.users.List()
	for _, u := range all {
		if u.SSOSub != "" {
			n++
		}
	}
	return n
}

// signIn is the browser sign-in and the device sign-on for sub of org: the status, whether
// a session was made, and each answer's body.
func (h cellHost) signIn(t *testing.T, sub, org, jti string) (browser, device *httptest.ResponseRecorder) {
	t.Helper()
	browser = signInRecorder(t, h.srv, h.idp, map[string]any{"sub": sub, "preferred_username": "u_" + sub, "roles": []string{}, "org_id": org})
	h.idp.SetClaims(deviceClaims(map[string]any{"sub": sub, "preferred_username": "u_" + sub, "jti": jti, "org_id": org}))
	device = httptest.NewRecorder()
	h.srv.handleNativeSignOn(device, signOnRequest(h.idp.IDToken()))
	return browser, device
}

func (h cellHost) taskState(t *testing.T, sub, org, id string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"sub": sub, "issuer": h.srv.ssoStore.Load().IssuerURL, "ref": "r", "done": true, "org_id": org}
	return signedTo(t, h.srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, id, body)
}

func sameAnswer(a, b *httptest.ResponseRecorder) bool {
	return a.Code == b.Code && a.Body.String() == b.Body.String() && (sessionCookieFrom(a) == nil) == (sessionCookieFrom(b) == nil)
}

// MH9 (KyIdentity tenancy spec, Cells, C5): two hosts, one per cell, poll one KyIdentity.
// Each caches only its own cell's orgs and keeps the other's as ID and cell. At the wrong
// host an org of the other cell gets, on every way in, the answer an org no host knows gets:
// browser sign-in, device sign-on, the org gate behind every credential, and a KyCalendar
// peer call. Its directory events are refused for good there, an uncached org's for now,
// and neither writes anything. The staff org is served in every cell.
func TestCellsServeOnlyTheirOwnOrgs(t *testing.T) {
	const (
		cellA, cellB = "us-east-1", "us-east-2"
		orgC         = "1a2b3c4d-0000-4000-8000-00000000000c" // cell A, listed late
		orgNone      = "1a2b3c4d-0000-4000-8000-00000000000d" // never listed
	)
	ctx := context.Background()
	orgs := newOrgsIdP(t)
	in := func(o suitedir.Org, cell string) suitedir.Org { o.HomeRegion = cell; return o }
	orgs.set(0, in(listed(orgA, "active", 1), cellA), in(listed(orgB, "active", 2), cellB), in(listed(staffOrg, "active", 3), cellB))
	a, b := newCellHost(t, cellA, orgs), newCellHost(t, cellB, orgs)
	a.poll(t)
	b.poll(t)

	// The poll: own orgs cached, the other cell's held as foreign, staff served in both.
	for _, c := range []struct {
		h          cellHost
		own, other string
	}{{a, orgA, orgB}, {b, orgB, orgA}} {
		if err := c.h.srv.orgServe(ctx, c.own); err != nil {
			t.Fatalf("own org: %v", err)
		}
		if o := orgRow(t, c.h.srv.tenancy, c.other); o.Known {
			t.Fatalf("the other cell's org is cached: %+v", o)
		}
		if foreign, err := tenancy.Foreign(ctx, c.h.srv.tenancy, c.other); err != nil || !foreign {
			t.Fatalf("the other cell's org is not held as foreign: %v %v", foreign, err)
		}
		if err := c.h.srv.orgServe(ctx, staffOrg); err != nil || !orgRow(t, c.h.srv.tenancy, staffOrg).Known {
			t.Fatalf("staff org not cached and served: %v", err)
		}
		if cursor, _ := tenancy.OrgCursor(ctx, c.h.srv.tenancy); cursor != 2 {
			t.Fatalf("cursor %d, want 2", cursor)
		}
	}

	// Directory events: each home cell provisions its member. KyIdentity sending b1's event
	// to cell A is refused for good (422, not a retry), and nothing is written.
	provisionMember(t, a.srv, a.srv.tenancy, "a1", orgA)
	provisionMember(t, b.srv, b.srv.tenancy, "b1", orgB)
	before := a.writes(t)
	if rec := postDirectory(t, a.srv, testSyncKey, "user.created", "ev-b1-at-a", 1, orgUser("b1", orgB, true)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the other cell's event: %d %s", rec.Code, rec.Body)
	}
	// An org no poll has listed yet is refused for now (503, KyIdentity retries), and
	// applies once the poll lists it in this cell.
	if rec := postDirectory(t, a.srv, testSyncKey, "user.created", "ev-c1", 1, orgUser("c1", orgC, true)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an uncached org's event: %d %s", rec.Code, rec.Body)
	}
	if after := a.writes(t); after != before {
		t.Fatalf("refused events wrote %d rows or accounts", after-before)
	}
	orgs.set(0, in(listed(orgA, "active", 1), cellA), in(listed(orgB, "active", 2), cellB), in(listed(staffOrg, "active", 3), cellB), in(listed(orgC, "active", 4), cellA))
	a.poll(t)
	provisionMember(t, a.srv, a.srv.tenancy, "c1", orgC)

	// Sign-in, device sign-on, the org gate and a peer call: b1 at cell A answers as a
	// stranger of an org nobody lists; at its own cell it is served.
	browser, device := a.signIn(t, "b1", orgB, "jti-b1-a")
	strangerBrowser, strangerDevice := a.signIn(t, "nobody", orgNone, "jti-nobody-a")
	if !sameAnswer(browser, strangerBrowser) || sessionCookieFrom(browser) != nil {
		t.Errorf("browser sign-in at the other cell: %d %q, an unknown org gets %d %q", browser.Code, browser.Body, strangerBrowser.Code, strangerBrowser.Body)
	}
	if !sameAnswer(device, strangerDevice) || device.Code == http.StatusOK {
		t.Errorf("device sign-on at the other cell: %d %q, an unknown org gets %d %q", device.Code, device.Body, strangerDevice.Code, strangerDevice.Body)
	}
	if browser, device := b.signIn(t, "b1", orgB, "jti-b1-b"); sessionCookieFrom(browser) == nil || device.Code != http.StatusOK {
		t.Errorf("b1 at its own cell: browser %d, device %d %s", browser.Code, device.Code, device.Body)
	}
	if browser, device := a.signIn(t, "a1", orgA, "jti-a1-a"); sessionCookieFrom(browser) == nil || device.Code != http.StatusOK {
		t.Errorf("a1 at its own cell: browser %d, device %d %s", browser.Code, device.Code, device.Body)
	}
	if got, unknown := a.srv.orgServe(ctx, orgB), a.srv.orgServe(ctx, orgNone); got == nil || got.Error() != unknown.Error() {
		t.Errorf("org gate at the other cell: %v, an unknown org gets %v", got, unknown)
	}
	peer, stranger := a.taskState(t, "b1", orgB, "task-b1-a"), a.taskState(t, "nobody", orgNone, "task-nobody-a")
	if !sameAnswer(peer, stranger) || answerCode(peer) != "no_user" {
		t.Errorf("peer call at the other cell: %d %s, an unknown org gets %d %s", peer.Code, peer.Body, stranger.Code, stranger.Body)
	}
	if rec := b.taskState(t, "b1", orgB, "task-b1-b"); answerCode(rec) == "no_user" {
		t.Errorf("peer call at b1's own cell: %d %s", rec.Code, rec.Body)
	}
}
