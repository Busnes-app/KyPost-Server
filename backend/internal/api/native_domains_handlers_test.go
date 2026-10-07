//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailDomainsAdminSetAndRelayDomains(t *testing.T) {
	srv := newDirectoryTestServer(t)
	const password = "long-password-for-domains"
	admin, err := srv.users.Create(context.Background(), "domains-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(context.Background(), "domains-member", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	mtoken, mcsrf := mintSessionForTest(srv, member.ID)
	call := func(method, path, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		}
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	mutations := []struct{ method, path string }{{"POST", "/api/admin/mail-domains"}, {"POST", "/api/admin/mail-domains/second.test/verify"}, {"DELETE", "/api/admin/mail-domains/second.test"}}
	for _, route := range mutations {
		for _, c := range []struct {
			token, csrf string
			want        int
		}{{"", "", 401}, {mtoken, mcsrf, 403}, {token, "", 403}, {token, csrf, 401}} {
			if w := call(route.method, route.path, c.token, c.csrf, "{}"); w.Code != c.want {
				t.Fatalf("auth %s %s: got %d want %d: %s", route.method, route.path, w.Code, c.want, w.Body)
			}
		}
	}
	if w := call("GET", "/api/admin/mail-domains", mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member listed domains", w.Code)
	}
	u := allocateNativeStateTestUser(t, srv, "https://idp.example", "one")
	lapsed := map[string]bool{}
	srv.nativeDomains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		set, err := srv.nativeDomains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." && !lapsed[d.Domain] {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, err
	})
	confirm := `{"password":"` + password + `"}`
	if w := call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"Second.test","password":"`+password+`"}`); w.Code != 409 {
		t.Fatal("uppercase domain accepted", w.Code, w.Body)
	}
	w := call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"second.test","password":"`+password+`"}`)
	var added struct {
		Domain, RecordName, RecordValue string
		Configured, Established         bool
	}
	if err = json.Unmarshal(w.Body.Bytes(), &added); w.Code != 200 || err != nil || added.RecordName != "_kypost-mail.second.test" || added.RecordValue == "" || !added.Configured || added.Established {
		t.Fatal("add", w.Code, w.Body)
	}
	if w = call("POST", "/api/admin/mail-domains/unknown.test/verify", token, csrf, confirm); w.Code != 409 {
		t.Fatal("unconfigured domain verified", w.Code, w.Body)
	}
	if w = call("POST", "/api/admin/mail-domains/second.test/verify", token, csrf, confirm); w.Code != 200 || !strings.Contains(w.Body.String(), `"established":true`) {
		t.Fatal("verify", w.Code, w.Body)
	}
	// The single-domain endpoint keeps serving the founding domain.
	if w = call("GET", "/api/admin/mail-domain", token, csrf, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"domain":"example.test"`) {
		t.Fatal("founding domain", w.Code, w.Body)
	}
	relay := func(body string) (int, mailmsg.DomainRelay) {
		w := call("PUT", "/api/admin/mail-relay", token, csrf, body)
		var got struct {
			Domain         string
			Domains        []string
			RetiredDomains []string
			Generation     string
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code == 200 && got.Domain != "example.test" {
			t.Fatal("relay founding domain", w.Body)
		}
		return w.Code, mailmsg.DomainRelay{Generation: got.Generation, Domains: got.Domains, RetiredDomains: got.RetiredDomains}
	}
	code, first := relay(`{"host":"smtp.example.test","smtpUsername":"login","smtpPassword":"secret","password":"` + password + `"}`)
	if code != 200 || !slices.Equal(first.Domains, []string{"example.test"}) {
		t.Fatal("legacy relay shape", code, first)
	}
	code, both := relay(`{"domains":["second.test","example.test"],"password":"` + password + `"}`)
	if code != 200 || both.Generation != first.Generation || !slices.Equal(both.Domains, []string{"example.test", "second.test"}) {
		t.Fatal("adding a relay domain", code, both)
	}
	if code, _ = relay(`{"domains":["example.test","unknown.test"],"password":"` + password + `"}`); code != 409 {
		t.Fatal("unverified relay domain accepted", code)
	}
	// A lapsed retained domain no longer blocks a credential rotation or the
	// relay check; a newly added domain still needs fresh proof, and at least
	// one domain must prove.
	lapsed["second.test"] = true
	code, rotated := relay(`{"host":"smtp.example.test","smtpUsername":"login","smtpPassword":"rotated","password":"` + password + `"}`)
	if code != 200 || rotated.Generation == first.Generation || !slices.Equal(rotated.Domains, both.Domains) {
		t.Fatal("lapsed retained domain blocked rotation", code, rotated)
	}
	if _, proofs, err := srv.nativeRelayCheckProfile(context.Background(), rotated.Generation, nil); err != nil || len(proofs) != 1 || proofs[0].Domain != "example.test" {
		t.Fatal("lapsed retained domain blocked the relay check", proofs, err)
	}
	if w = call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"third.test","password":"`+password+`"}`); w.Code != 200 {
		t.Fatal("add third", w.Code, w.Body)
	}
	lapsed["third.test"] = true
	if code, _ = relay(`{"domains":["example.test","second.test","third.test"],"password":"` + password + `"}`); code != 409 {
		t.Fatal("unproven new relay domain accepted", code)
	}
	lapsed["example.test"] = true
	if code, _ = relay(`{"host":"smtp.example.test","smtpUsername":"login","smtpPassword":"rotated","password":"` + password + `"}`); code != 409 {
		t.Fatal("relay rotated with no fresh proof", code)
	}
	if _, _, err := srv.nativeRelayCheckProfile(context.Background(), rotated.Generation, nil); err == nil {
		t.Fatal("relay check passed with no fresh proof")
	}
	lapsed["example.test"], lapsed["second.test"] = false, false
	if w = call("DELETE", "/api/admin/mail-domains/third.test", token, csrf, confirm); w.Code != 200 {
		t.Fatal("retire third", w.Code, w.Body)
	}
	first = rotated
	// A queued job From the second domain blocks its removal.
	a, _, err := srv.ssoLifecycle.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(srv.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	master, err := cryptutil.LoadKey(filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil {
		t.Fatal(err)
	}
	const jobID = "07a7cd86-6d9d-4b91-890d-c6d78d096099"
	raw := []byte("From: one@second.test\r\nTo: receiver@outside.test\r\nSubject: queued\r\n\r\nbody\r\n")
	if err = box.QueueOutbound(context.Background(), master, jobID, mailbox.OutboundJob{From: "one@second.test", RelayGeneration: first.Generation, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"receiver@outside.test"}, Raw: raw}}}); err != nil {
		t.Fatal(err)
	}
	if code, _ = relay(`{"domains":["example.test"],"password":"` + password + `"}`); code != 409 {
		t.Fatal("removed a relay domain with a queued job", code)
	}
	if w = call("DELETE", "/api/admin/mail-domains/second.test", token, csrf, confirm); w.Code != 409 {
		t.Fatal("retired a domain with a queued job", w.Code, w.Body)
	}
	if err = box.QuarantineOutbound(context.Background(), jobID, 0); err != nil {
		t.Fatal(err)
	}
	code, removed := relay(`{"domains":["example.test"],"password":"` + password + `"}`)
	if code != 200 || removed.Generation != first.Generation || !slices.Equal(removed.RetiredDomains, []string{"second.test"}) {
		t.Fatal("relay domain removal", code, removed)
	}
	if w = call("DELETE", "/api/admin/mail-domains/example.test", token, csrf, confirm); w.Code != 409 {
		t.Fatal("retired a domain with an active address", w.Code, w.Body)
	}
	if w = call("DELETE", "/api/admin/mail-domains/second.test", token, csrf, confirm); w.Code != 200 {
		t.Fatal("retire", w.Code, w.Body)
	}
	w = call("GET", "/api/admin/mail-domains", token, csrf, "")
	var list struct {
		Founding string
		Domains  []struct {
			Domain              string
			Configured, Retired bool
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || list.Founding != "example.test" || len(list.Domains) != 3 || list.Domains[1].Domain != "second.test" || !list.Domains[1].Retired || list.Domains[1].Configured {
		t.Fatal("list", w.Code, w.Body)
	}
	// Re-adding a retired domain takes the same confirmation, starts unproven
	// and returns to the relay with the generation kept once verified.
	if w = call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"second.test"}`); w.Code != 401 {
		t.Fatal("re-add without confirmation", w.Code, w.Body)
	}
	if w = call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"second.test","password":"`+password+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"established":false`) || !strings.Contains(w.Body.String(), `"retired":false`) {
		t.Fatal("re-add", w.Code, w.Body)
	}
	lapsed["second.test"] = true
	if code, _ = relay(`{"domains":["example.test","second.test"],"password":"` + password + `"}`); code != 409 {
		t.Fatal("unproven re-added domain joined the relay", code)
	}
	lapsed["second.test"] = false
	if w = call("POST", "/api/admin/mail-domains/second.test/verify", token, csrf, confirm); w.Code != 200 {
		t.Fatal("verify re-added", w.Code, w.Body)
	}
	code, readded := relay(`{"domains":["example.test","second.test"],"password":"` + password + `"}`)
	if code != 200 || readded.Generation != removed.Generation || !slices.Equal(readded.Domains, []string{"example.test", "second.test"}) || len(readded.RetiredDomains) != 0 {
		t.Fatal("relay re-add", code, readded)
	}
	if w = call("GET", "/api/admin/mail-domains", token, csrf, ""); !strings.Contains(w.Body.String(), `"founding":"example.test"`) {
		t.Fatal("re-add moved the founding domain", w.Body)
	}
	if err = sso.WriteNativeV1ForTest(srv.configDir, filepath.Join(config.SecretDir(), "native-relay.key")); err != nil {
		t.Fatal(err)
	}
	if w = call("GET", "/api/admin/mail-domains", token, csrf, ""); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "migrate-native") {
		t.Fatal("migration refusal", w.Code, w.Body)
	}
}

func TestNativeMailDomainsKySignOnRequiresBoundStepUp(t *testing.T) {
	srv := newDirectoryTestServer(t)
	admin, err := srv.users.CreateSSOUser("domains-sso-admin", users.RoleAdmin, "domains-admin-sub", "", "")
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	srv.sessMu.Lock()
	session := srv.sessions[token]
	session.SSOKySignOn, session.SSOAppAdmin = true, true
	session.SSO = sso.SessionIdentity{Issuer: srv.ssoStore.Load().IssuerURL, Subject: "domains-admin-sub"}
	srv.sessions[token] = session
	srv.sessMu.Unlock()
	if _, err = srv.nativeDomains.ConfigureDomain(context.Background(), "example.test", srv.ssoStore.Load().IssuerURL); err != nil {
		t.Fatal(err)
	}
	if err = srv.nativeDomains.RetireDomain(context.Background(), "example.test", srv.stateDir, filepath.Join(t.TempDir(), "absent.key")); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/admin/mail-domains", `{"domain":"example.test"}`},
		{"POST", "/api/admin/mail-domains/example.test/verify", `{}`},
		{"DELETE", "/api/admin/mail-domains/example.test", `{}`},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		challengeFrom(t, w)
	}
	if set, err := srv.nativeDomains.ReadSet(); err != nil || len(set.Domains) != 0 || !slices.Equal(set.Retired, []string{"example.test"}) {
		t.Fatal("step-up refusal mutated the set", set, err)
	}
}
