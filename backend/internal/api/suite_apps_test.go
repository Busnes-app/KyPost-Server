package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// suiteIdP is KyIdentity's suite directory: it answers each sub with its own app, or fails.
type suiteIdP struct {
	*httptest.Server
	mu   sync.Mutex
	subs []string
	fail bool
}

// newSuiteIdP serves plain HTTP on loopback, which suitedir accepts as an issuer.
func newSuiteIdP(t *testing.T) *suiteIdP {
	idp := &suiteIdP{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); r.URL.Path != "/api/suite/apps" || !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		sub := r.URL.Query().Get("sub")
		idp.mu.Lock()
		idp.subs = append(idp.subs, sub)
		fail := idp.fail
		idp.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"apps":[{"id":"kypost","name":"KyPost","url":"https://mail.example.com/api/auth/oidc/login"},{"id":"app-%s","name":"App %s","url":"https://%s.example.com/"}]}`, sub, sub, sub)
	}))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *suiteIdP) setFail(fail bool) {
	idp.mu.Lock()
	idp.fail = fail
	idp.mu.Unlock()
}

func (idp *suiteIdP) calls() []string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return append([]string(nil), idp.subs...)
}

// suiteServer is a KyPost whose SSO points at idp with the given client secret.
func suiteServer(t *testing.T, idp *suiteIdP, secret string) *Server {
	t.Helper()
	s := newTestServer(t)
	// The trailing slash is how operators often type it; links store the trimmed issuer.
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL + "/", ClientID: "kypost", ClientSecret: secret}); err != nil {
		t.Fatal(err)
	}
	return s
}

func ssoUser(t *testing.T, s *Server, name, issuer, sub string) users.User {
	t.Helper()
	u, err := s.users.Create(context.Background(), name, name+"-testpassword", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if sub != "" {
		if err := s.users.LinkSSOIssuer(issuer, u.ID, sub, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// suiteApps calls GET /api/suite/apps through the router as u and returns the app IDs.
func suiteApps(t *testing.T, s *Server, u users.User) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/suite/apps", nil)
	authRequestAs(s, req, u.ID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		Apps []struct{ ID, Name, URL string } `json:"apps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Apps == nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	ids := []string{}
	for _, a := range body.Apps {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestSuiteAppsRequiresSession(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/suite/apps", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestSuiteAppsForAKyIdentityUser(t *testing.T) {
	idp := newSuiteIdP(t)
	s := suiteServer(t, idp, "suite-secret")
	ann := ssoUser(t, s, "ann", idp.URL, "sub-ann")
	if got := suiteApps(t, s, ann); fmt.Sprint(got) != "[kypost app-sub-ann]" {
		t.Fatalf("apps %v", got)
	}
	if fmt.Sprint(idp.calls()) != "[sub-ann]" {
		t.Fatalf("KyIdentity saw %v", idp.calls())
	}
}

func TestSuiteAppsEmptyWithoutAKyIdentityLink(t *testing.T) {
	idp := newSuiteIdP(t)
	s := suiteServer(t, idp, "suite-secret")
	local := ssoUser(t, s, "loc", "", "")
	revoked := ssoUser(t, s, "rev", idp.URL, "sub-rev")
	if _, err := s.users.RevokeSSOLinksNotFrom("https://moved.example.com"); err != nil {
		t.Fatal(err)
	}
	other := ssoUser(t, s, "oth", "https://other.example.com", "sub-oth")
	for _, u := range []users.User{local, other, revoked} {
		if got := suiteApps(t, s, u); len(got) != 0 {
			t.Fatalf("%s: %v", u.Username, got)
		}
	}
	// SSO switched off: a linked user gets nothing either.
	linked := ssoUser(t, s, "lnk", idp.URL, "sub-lnk")
	st := s.ssoStore.Load()
	st.Enabled = false
	if err := s.ssoStore.Save(st); err != nil {
		t.Fatal(err)
	}
	if got := suiteApps(t, s, linked); len(got) != 0 {
		t.Fatalf("sso off: %v", got)
	}
	if calls := idp.calls(); len(calls) != 0 {
		t.Fatalf("KyIdentity was asked: %v", calls)
	}
}

func TestSuiteAppsEmptyWhenKyIdentityFails(t *testing.T) {
	idp := newSuiteIdP(t)
	idp.setFail(true)
	s := suiteServer(t, idp, "suite-secret")
	if got := suiteApps(t, s, ssoUser(t, s, "ann", idp.URL, "sub-ann")); len(got) != 0 {
		t.Fatalf("apps %v", got)
	}
	// Refused credentials too.
	idp.setFail(false)
	bad := suiteServer(t, idp, "wrong")
	if got := suiteApps(t, bad, ssoUser(t, bad, "bob", idp.URL, "sub-bob")); len(got) != 0 {
		t.Fatalf("apps %v", got)
	}
}

func TestSuiteAppsCachedPerUser(t *testing.T) {
	idp := newSuiteIdP(t)
	s := suiteServer(t, idp, "suite-secret")
	ann, bob := ssoUser(t, s, "ann", idp.URL, "sub-ann"), ssoUser(t, s, "bob", idp.URL, "sub-bob")
	for range 3 {
		if got := suiteApps(t, s, ann); fmt.Sprint(got) != "[kypost app-sub-ann]" {
			t.Fatalf("ann %v", got)
		}
	}
	// Bob never gets Ann's cached list.
	if got := suiteApps(t, s, bob); fmt.Sprint(got) != "[kypost app-sub-bob]" {
		t.Fatalf("bob %v", got)
	}
	if fmt.Sprint(idp.calls()) != "[sub-ann sub-bob]" {
		t.Fatalf("KyIdentity saw %v", idp.calls())
	}
	// A failure is cached as well, so an outage is not asked again on every page load.
	idp.setFail(true)
	carl := ssoUser(t, s, "carl", idp.URL, "sub-carl")
	suiteApps(t, s, carl)
	suiteApps(t, s, carl)
	if n := len(idp.calls()); n != 3 {
		t.Fatalf("%d calls after a cached failure", n)
	}
	idp.setFail(false)
	// New SSO client settings never serve the old list.
	st := s.ssoStore.Load()
	st.ClientSecret = "suite-secret"
	st.IssuerURL = idp.URL
	if err := s.ssoStore.Save(st); err != nil {
		t.Fatal(err)
	}
	suiteApps(t, s, ann)
	if n := len(idp.calls()); n != 4 {
		t.Fatalf("%d calls after a settings change", n)
	}
	// Expired entries are fetched again.
	old := suiteAppsTTL
	suiteAppsTTL = -time.Second
	t.Cleanup(func() { suiteAppsTTL = old })
	suiteApps(t, s, bob)
	suiteApps(t, s, bob)
	if fmt.Sprint(idp.calls()[4:]) != "[sub-bob sub-bob]" {
		t.Fatalf("KyIdentity saw %v", idp.calls())
	}
}
