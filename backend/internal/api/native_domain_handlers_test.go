package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeMailDomainAdminProofRoutes(t *testing.T) {
	srv := newDirectoryTestServer(t)
	const password = "long-password-for-domain"
	admin, err := srv.users.Create(context.Background(), "mail-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(context.Background(), "mail-member", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	mtoken, mcsrf := mintSessionForTest(srv, member.ID)
	call := func(method, path, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/mail-domain"+path, bytes.NewBufferString(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		}
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	for _, route := range []struct{ method, path string }{{"PUT", ""}, {"POST", "/verify"}} {
		for _, c := range []struct {
			token, csrf, body string
			want              int
		}{{"", "", "{}", 401}, {mtoken, mcsrf, "{}", 403}, {token, "", "{}", 403}, {token, csrf, "{}", 403}} {
			if w := call(route.method, route.path, c.token, c.csrf, c.body); w.Code != c.want {
				t.Fatalf("auth %s %d %s", route.path, w.Code, w.Body)
			}
		}
	}
	if w := call("GET", "", mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member read", w.Code)
	}
	if w := call("GET", "", token, csrf, ""); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body)
	}
	raw, _ := json.Marshal(map[string]string{"domain": "example.test", "password": password})
	if w := call("PUT", "", token, csrf, string(raw)); w.Code != 200 {
		t.Fatal("configure", w.Code, w.Body)
	}
	srv.nativeDomains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		d, err := srv.nativeDomains.Read()
		if name != d.RecordName()+"." {
			t.Fatal(name)
		}
		return []string{d.RecordValue()}, err
	})
	raw, _ = json.Marshal(map[string]string{"password": password})
	w := call("POST", "/verify", token, csrf, string(raw))
	if w.Code != 200 {
		t.Fatal("verify", w.Code, w.Body)
	}
	var result struct {
		ReceivingEnabled bool `json:"receivingEnabled"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.ReceivingEnabled {
		t.Fatal("receiver activated", err)
	}
	// Unmigrated storage gets its own remediation on every domain route.
	if err = sso.WriteNativeV1ForTest(srv.configDir, srv.configDir+"/absent-relay.key"); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]string{"domain": "example.test", "password": password})
	for _, c := range []struct{ method, path, body string }{{"GET", "", ""}, {"PUT", "", string(raw)}, {"POST", "/verify", `{"password":"` + password + `"}`}} {
		if w := call(c.method, c.path, token, csrf, c.body); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "migrate-native") {
			t.Fatal("migration refusal", c.method, c.path, w.Code, w.Body)
		}
	}
}

func TestNativeAccountIssuerFenceUsesVerifiedProvenance(t *testing.T) {
	srv := newDirectoryTestServer(t)
	const issuer = "https://original.example.test"
	u := allocateNativeStateTestUser(t, srv, issuer, "same-sub")
	var err error
	// Current mutable configuration is deliberately B; the passed event issuer
	// remains A. Neither B login nor B directory update may resolve A's owner.
	if err = srv.ssoStore.Save(sso.SSOSettings{IssuerURL: "https://replacement.example.test"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if _, err = srv.resolveSSOUser(w, srv.ssoStore.Load(), &sso.SSOTokenClaims{Issuer: "https://replacement.example.test", Sub: "same-sub"}); err == nil {
		t.Fatal("new issuer authenticated old mailbox")
	}
	active := false
	resource := sso.DirectoryUser{ID: "same-sub", Active: &active}
	if _, err = srv.applyDirectoryUser("https://replacement.example.test", resource); err == nil {
		t.Fatal("new issuer offboarded old mailbox")
	}
	current, err := srv.users.GetBySSOSub("same-sub")
	if err != nil || !current.Active || current.ID != u.ID {
		t.Fatal("foreign issuer changed owner", current, err)
	}
	if _, err = srv.applyDirectoryUser(issuer, resource); err != nil {
		t.Fatal("verified original issuer refused after settings switch", err)
	}
	current, err = srv.users.GetBySSOSub("same-sub")
	if err != nil || current.Active {
		t.Fatal("verified offboarding failed", current, err)
	}
}

func TestNativeMailDomainKySignOnRequiresBoundStepUp(t *testing.T) {
	srv := newDirectoryTestServer(t)
	admin, err := srv.users.CreateSSOUser("mail-sso-admin", users.RoleAdmin, "mail-admin-sub", "", "")
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	srv.sessMu.Lock()
	session := srv.sessions[token]
	session.SSOKySignOn = true
	session.SSOAppAdmin = true
	session.SSO = sso.SessionIdentity{Issuer: srv.ssoStore.Load().IssuerURL, Subject: "mail-admin-sub"}
	srv.sessions[token] = session
	srv.sessMu.Unlock()
	for _, route := range []struct{ method, path, body string }{{"PUT", "", `{"domain":"example.test"}`}, {"POST", "/verify", `{}`}} {
		req := httptest.NewRequest(route.method, "/api/admin/mail-domain"+route.path, strings.NewReader(route.body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		challengeFrom(t, w)
	}
	d, err := srv.nativeDomains.Read()
	if err != nil || d.Domain != "" {
		t.Fatal("step-up refusal mutated claim", d, err)
	}
}
