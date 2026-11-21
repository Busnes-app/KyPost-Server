//go:build linux

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// bindOrg makes an existing fixture account org-bound as PR 2's provisioning would:
// users.json OrgID, user_orgs and subject_orgs. Fixtures built for single mode reach the
// multi paths this way; the write bypasses the store on purpose (OrgID is write-once).
func bindOrg(t *testing.T, srv *Server, db *sql.DB, u users.User, org string) users.User {
	t.Helper()
	path := filepath.Join(srv.configDir, "users.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, x := range f["users"].([]any) {
		if rec := x.(map[string]any); rec["id"] == u.ID {
			rec["orgId"] = org
		}
	}
	raw, _ = json.Marshal(f)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if u.SSOSub != "" {
		if err := tenancy.BindSubject(ctx, db, u.SSOIssuer, u.SSOSub, org); err != nil {
			t.Fatal(err)
		}
	}
	if err := tenancy.BindUser(ctx, db, u.ID, org); err != nil {
		t.Fatal(err)
	}
	got, err := srv.users.Get(u.ID)
	if err != nil || got.OrgID != org {
		t.Fatalf("bindOrg: %+v %v", got, err)
	}
	return got
}

func TestOrgAdmit(t *testing.T) {
	single := &Server{}
	if !single.orgAdmit(AuthContext{}, users.User{OrgID: orgB}) {
		t.Fatal("single mode compared orgs")
	}
	multi := &Server{tenancy: &sql.DB{}}
	for _, c := range []struct {
		caller, target string
		want           bool
	}{{orgA, orgA, true}, {orgA, orgB, false}, {"", "", false}, {orgA, "", false}, {"", orgA, false}} {
		if got := multi.orgAdmit(AuthContext{OrgID: c.caller}, users.User{OrgID: c.target}); got != c.want {
			t.Errorf("orgAdmit(%q, %q) = %v, want %v", c.caller, c.target, got, c.want)
		}
	}
	for body, want := range map[string]bool{
		`{"org_id":"` + orgA + `"}`: true, `{"org_id":"` + orgB + `"}`: false, `{}`: false,
		`{"org_id":"` + strings.ToUpper(orgA) + `"}`: false, `{"org_id":7}`: false, `not json`: false,
	} {
		if got := multi.peerOrgAdmit([]byte(body), users.User{OrgID: orgA}); got != want {
			t.Errorf("peerOrgAdmit(%s) = %v, want %v", body, got, want)
		}
	}
	if !single.peerOrgAdmit([]byte(`{}`), users.User{}) {
		t.Fatal("single mode read a peer org_id")
	}
}

// The extra-mailbox hop: the ledger admits the caller's own mailbox, and orgAdmit refuses
// it anyway for an AuthContext of another org, with the unknown mailbox's answer.
func TestTenancyMailboxHopNeedsTheOrg(t *testing.T) {
	fx, _ := newOrgFixture(t)
	admit := func(org, mailbox string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/inbox", nil)
		req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: fx.a.ID, OrgID: org}))
		w := httptest.NewRecorder()
		if _, ok := fx.srv.admitMailbox(w, req, mailbox); ok {
			w.WriteHeader(299)
		}
		return w
	}
	if w := admit(orgA, fx.a.extra); w.Code != 299 {
		t.Fatalf("own org: %d %s", w.Code, w.Body)
	}
	unknown := admit(orgA, mailboxHeaderCase.missing)
	for _, org := range []string{orgB, ""} {
		if w := admit(org, fx.a.extra); w.Code != unknown.Code || w.Body.String() != unknown.Body.String() {
			t.Errorf("org %q: %d %s, want the unknown mailbox's %d %s", org, w.Code, w.Body, unknown.Code, unknown.Body)
		}
	}
}

// Every credential path carries the account's org, and multi mode refuses an account
// without one exactly as a dead session.
func TestTenancyCredentialsCarryTheOrg(t *testing.T) {
	fx, _ := newOrgFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(fx.srv, req, fx.a.ID)
	if ac, ok, err := fx.srv.currentUser(req); !ok || err != nil || ac.OrgID != orgA {
		t.Fatalf("session AuthContext %+v %v %v", ac, ok, err)
	}
	orgless, err := fx.srv.users.Create(context.Background(), "orgless", "orgless-testpassword", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(fx.srv, req, orgless.ID)
	if ac, ok, err := fx.srv.currentUser(req); ok || err != nil {
		t.Fatalf("an account with no org got a session: %+v %v", ac, err)
	}
	for _, path := range []string{"/api/inbox", "/api/contacts"} {
		w := fx.call(orgMember{User: orgless}, http.MethodGet, path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s as an org-less account: %d, want 401", path, w.Code)
		}
	}
	if org, ok := fx.srv.deviceOrg(orgless.ID); ok || org != "" {
		t.Fatal("deviceOrg admitted an org-less account")
	}
	if org, ok := fx.srv.deviceOrg(fx.a.ID); !ok || org != orgA {
		t.Fatalf("deviceOrg(member) = %q %v", org, ok)
	}
}

// Inbound peer bodies (KyCalendar iTIP send and task state, KyPulse alerts) must carry
// the account's org_id in multi mode; none, another org's or a malformed one answers like
// an unknown subject. Single mode reads none of it.
func TestTenancyInboundPeersNeedTheAccountOrg(t *testing.T) {
	f := newPulseFixture(t)
	single := pulseSend(t, f.srv, pulseSecret, "kypulse.alert.send", "single-org", alertBody(func(b map[string]any) { b["org_id"] = orgA }))
	if single.Code != http.StatusBadRequest {
		t.Fatalf("single mode accepted a KyPulse org_id field: %d %s", single.Code, single.Body)
	}
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "single-itip", sendBody(func(b map[string]any) { b["org_id"] = orgB })); rec.Code != http.StatusAccepted {
		t.Fatalf("single mode iTIP with an ignored org_id: %d %s", rec.Code, rec.Body)
	}
	f.delivered(t)
	db := makeMultiTenant(t, f.srv)
	u, _ := f.srv.users.Get(f.userID)
	bindOrg(t, f.srv, db, u, orgA)
	for name, org := range map[string]any{"missing": nil, "other org": orgB, "uppercase": strings.ToUpper(orgA), "number": 1} {
		set := func(b map[string]any) {
			if org != nil {
				b["org_id"] = org
			}
		}
		id := strings.ReplaceAll(name, " ", "-")
		if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "itip-"+id, sendBody(set)); rec.Code != http.StatusNotFound || answerCode(rec) != "no_user" {
			t.Errorf("iTIP send, %s org_id: %d %s", name, rec.Code, rec.Body)
		}
		state := map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "ref": "r", "done": true}
		set(state)
		if rec := signedTo(t, f.srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, "task-"+id, state); rec.Code != http.StatusNotFound || answerCode(rec) != "no_user" {
			t.Errorf("task state, %s org_id: %d %s", name, rec.Code, rec.Body)
		}
		if rec := pulseSend(t, f.srv, pulseSecret, "kypulse.alert.send", "pulse-"+id, alertBody(set)); rec.Code != http.StatusNotFound || answerCode(rec) != "no_user" {
			t.Errorf("KyPulse alert, %s org_id: %d %s", name, rec.Code, rec.Body)
		}
	}
	noDelivery(t, f.kycalFixture, "refused peer bodies")
	own := func(b map[string]any) { b["org_id"] = orgA }
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "itip-own", sendBody(own)); rec.Code != http.StatusAccepted {
		t.Fatalf("iTIP send with the account's org: %d %s", rec.Code, rec.Body)
	}
	if rec := pulseSend(t, f.srv, pulseSecret, "kypulse.alert.send", "pulse-own", alertBody(own)); answerCode(rec) == "no_user" {
		t.Fatalf("KyPulse alert with the account's org: %d %s", rec.Code, rec.Body)
	}
}

// Outbound peer bodies carry the caller's org_id where the peer's spec reads it
// (KyCalendar, KyDrive), and only in multi mode.
func TestTenancyOutboundPeersCarryTheOrg(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	t.Cleanup(peer.Close)
	last := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies[len(bodies)-1]
	}
	f := newKyCalFixture(t)
	base, _ := url.Parse(peer.URL)
	f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, peer.Client().Transport))
	away := config.OutOfOffice{}
	f.srv.syncOOOCalendar(context.Background(), f.userID, away)
	if _, has := last()["org_id"]; has {
		t.Fatalf("single mode sent an org_id: %v", last())
	}
	db := makeMultiTenant(t, f.srv)
	u, _ := f.srv.users.Get(f.userID)
	u = bindOrg(t, f.srv, db, u, orgA)
	f.srv.syncOOOCalendar(context.Background(), f.userID, away)
	if last()["org_id"] != orgA {
		t.Fatalf("multi mode KyCalendar body: %v", last())
	}
	fields := map[string]any{}
	if _, err := driveEvent(u, fields); err != nil || fields["org_id"] != orgA {
		t.Fatalf("KyDrive fields %v %v", fields, err)
	}
	fields = map[string]any{}
	if _, err := driveEvent(users.User{SSOSub: "s", SSOIssuer: "i"}, fields); err != nil || fields["org_id"] != nil {
		t.Fatalf("single-mode KyDrive fields %v %v", fields, err)
	}
}

// KyMessages has no spec for org_id on its bodies, so a multi install offers no chat and
// sends KyMessages nothing.
func TestTenancyKyMessagesIsOffInMultiMode(t *testing.T) {
	f := newChatFixture(t)
	if code, _, _ := f.call(t, f.ann, "/api/kymessage/people", `{"addresses":["bob@example.com"]}`); code != http.StatusOK {
		t.Fatalf("single mode people: %d", code)
	}
	db := makeMultiTenant(t, f.srv)
	ann := bindOrg(t, f.srv, db, f.ann, orgA)
	for _, path := range []string{"/api/kymessage/people", "/api/kymessage/chat", "/api/kymessage/status"} {
		if code, out, _ := f.call(t, ann, path, `{"addresses":["bob@example.com"],"address":"bob@example.com"}`); code != http.StatusConflict || out["code"] != "kymessage_off" {
			t.Errorf("%s in multi mode: %d %v", path, code, out)
		}
	}
	f.console.mu.Lock()
	n := len(f.console.bodies)
	f.console.mu.Unlock()
	f.srv.syncOOOMessages(context.Background(), ann.ID, config.OutOfOffice{})
	f.console.mu.Lock()
	defer f.console.mu.Unlock()
	if len(f.console.bodies) != n {
		t.Fatal("multi mode sent KyMessages a presence body")
	}
}

// Directory answers keep only people this install has bound to the caller's org.
func TestTenancyPeopleSearchKeepsTheCallersOrg(t *testing.T) {
	idp := newPeopleIdP(t)
	s := newTestServer(t)
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL + "/", ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	ann := ssoUser(t, s, "ann", idp.URL+"/", "sub-ann")
	db := makeMultiTenant(t, s)
	ann = bindOrg(t, s, db, ann, orgA)
	if code, body := people(t, s, ann, "Ali"); code != 200 || body != `{"people":[]}` {
		t.Fatalf("an unbound subject was kept: %d %s", code, body)
	}
	s.suitePeople.m = nil
	if err := tenancy.BindSubject(context.Background(), db, idp.URL+"/", "s-1", orgB); err != nil {
		t.Fatal(err)
	}
	if code, body := people(t, s, ann, "Ali"); code != 200 || body != `{"people":[]}` {
		t.Fatalf("another org's subject was kept: %d %s", code, body)
	}
	other := newPeopleIdP(t)
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: other.URL + "/", ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.BindSubject(context.Background(), db, other.URL+"/", "s-1", orgA); err != nil {
		t.Fatal(err)
	}
	ann2 := bindOrg(t, s, db, ssoUser(t, s, "ann2", other.URL+"/", "sub-ann2"), orgA)
	if code, body := people(t, s, ann2, "Ali"); code != 200 || !strings.Contains(body, "alice@example.com") {
		t.Fatalf("a subject of the caller's org was dropped: %d %s", code, body)
	}
}

// Address moves between mailboxes stay in one org: a reassign or a quarantine release to
// the current owner refuses a target whose org differs from any holder's.
func TestTenancyAddressMovesStayInOneOrg(t *testing.T) {
	fx, _ := newOrgFixture(t)
	address := "member-a-box@example.test"
	for _, c := range []struct {
		name, mailbox string
		want          bool
	}{{"same account", fx.a.extra, true}, {"same org", fx.other.extra, true}, {"other org", fx.b.extra, false}, {"unknown mailbox", mailboxHeaderCase.missing, false}} {
		ok, err := fx.srv.addressOneOrg(address, c.mailbox)
		if err != nil || ok != c.want {
			t.Errorf("%s: %v %v, want %v", c.name, ok, err, c.want)
		}
	}
	if ok, err := fx.srv.addressOneOrg("never-held@example.test", fx.a.extra); ok || err != nil {
		t.Errorf("an address with no history: %v %v", ok, err)
	}
	if _, err := fx.srv.addressOneOrg("not an address", fx.a.extra); err == nil {
		t.Error("a malformed address was not refused as invalid")
	}
	if ok, err := (&Server{}).addressOneOrg("x", "y"); !ok || err != nil {
		t.Fatal("single mode compared orgs")
	}
	// A users store that cannot be read is an error the operator can retry, not an org
	// refusal.
	if err := os.WriteFile(filepath.Join(fx.srv.configDir, "users.json"), []byte("{damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := fx.srv.addressOneOrg(address, fx.a.extra); ok || err == nil {
		t.Fatalf("an unreadable users store: %v %v, want an error", ok, err)
	}
}

// HR mailbox reservation is off in multi mode (K6): 404 before the signature is read.
func TestTenancyHRMailboxIsOff(t *testing.T) {
	fx, _ := newOrgFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/sync/hr-mailbox", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	fx.srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("HR in multi mode: %d %s", w.Code, w.Body)
	}
}
