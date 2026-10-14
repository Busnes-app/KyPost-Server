package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Every user-management mutation and the SSO settings write is refused without
// the acting admin's step-up and runs with it.
func TestAdminMutationsRequireStepUp(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	allowLoopbackOutboundForTest(t)
	dav := httptest.NewServer(http.NotFoundHandler())
	defer dav.Close()
	send := func(method, path string, fields map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(fields)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		authRequestAs(srv, req, admin.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	u := "/api/users/" + member.ID
	for _, c := range []struct {
		method, path string
		body         map[string]any
		want         int
	}{
		{"POST", "/api/users", map[string]any{"username": "stepup-new", "password": "stepup-new-password-1234"}, 201},
		{"PUT", u, map[string]any{"role": "user"}, 200},
		{"POST", u + "/reset-password", map[string]any{"password": "stepup-temp-password-1234"}, 200},
		{"POST", u + "/clear-mfa", nil, 200},
		{"PUT", u + "/send-quota", map[string]any{"sendQuota": map[string]int{"messagesPerHour": 1, "recipientsPerDay": 1}}, 200},
		{"POST", u + "/deactivate", nil, 200},
		{"POST", u + "/reactivate", nil, 200},
		{"PUT", u + "/imap-config", map[string]any{"host": "imap.example.test", "username": "m", "password": "imap-pw"}, 200},
		{"DELETE", u + "/imap-config", nil, 200},
		{"PUT", u + "/carddav-client", map[string]any{"serverUrl": dav.URL + "/dav/", "username": "m", "password": "dav-pw"}, 200},
		{"DELETE", u + "/carddav-client", nil, 200},
		{"PUT", "/api/admin/sso", map[string]any{"enabled": false, "clientId": "stepup"}, 200},
	} {
		body := map[string]any{}
		for k, v := range c.body {
			body[k] = v
		}
		if rec := send(c.method, c.path, body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s without step-up: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
		body["adminPassword"] = "wrong-password-entirely"
		if rec := send(c.method, c.path, body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a wrong credential: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
		body["adminPassword"] = "session-tester-testpassword"
		if rec := send(c.method, c.path, body); rec.Code != c.want {
			t.Fatalf("%s %s with step-up: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if _, err := srv.users.GetByUsername("stepup-new"); err != nil {
		t.Fatal("confirmed create did not create the user", err)
	}
}

// A KySignOn admin session confirms the exact request with KySignOn.
func TestAdminStepUpKySignOnSession(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	if _, err := srv.users.ClearMustChangePassword(admin.ID); err != nil {
		t.Fatal(err)
	}
	srv.sessMu.Lock()
	srv.sessions["sso-admin"] = Session{UserID: admin.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true}
	srv.sessMu.Unlock()
	call := func(path, challenge string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: "sso-admin"})
		req.Header.Set("X-CSRF-Token", "sso-csrf")
		if challenge != "" {
			req.Header.Set(stepUpHeader, challenge)
		}
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	path := "/api/users/" + member.ID + "/deactivate"
	rec := call(path, "")
	var refusal struct{ Error, Challenge string }
	if rec.Code != 403 || json.Unmarshal(rec.Body.Bytes(), &refusal) != nil || refusal.Error != "sso_step_up_required" {
		t.Fatal("KySignOn session skipped step-up", rec.Code, rec.Body)
	}
	srv.stepUpMu.Lock()
	c := srv.stepUps[refusal.Challenge]
	c.verified = true
	srv.stepUps[refusal.Challenge] = c
	srv.stepUpMu.Unlock()
	if rec = call("/api/users/"+member.ID+"/clear-mfa", refusal.Challenge); rec.Code != 403 {
		t.Fatal("grant spent on a different action", rec.Code, rec.Body)
	}
	if rec = call(path, refusal.Challenge); rec.Code != 200 {
		t.Fatal("confirmed KySignOn deactivate", rec.Code, rec.Body)
	}
}

// The client secret is write-only, and a blank one is kept only for the
// issuer and client it was issued for.
func TestAdminSSOClientSecretIsWriteOnly(t *testing.T) {
	srv := newTestServer(t)
	stored := sso.SSOSettings{IssuerURL: "https://auth.example.test", ClientID: "kypost", ClientSecret: "top-secret"}
	if err := srv.ssoStore.Save(stored); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.handleAdminSSOGet(rec, httptest.NewRequest("GET", "/api/admin/sso", nil))
	if strings.Contains(rec.Body.String(), "top-secret") || !strings.Contains(rec.Body.String(), `"clientSecretSet":true`) {
		t.Fatal("GET exposed the secret or hid that one is set:", rec.Body)
	}

	put := func(s sso.SSOSettings) string {
		raw, _ := json.Marshal(s)
		rec := httptest.NewRecorder()
		srv.handleAdminSSOPut(rec, httptest.NewRequest("PUT", "/api/admin/sso", bytes.NewReader(raw)))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "top-secret") {
			t.Fatal("PUT", rec.Code, rec.Body)
		}
		return srv.ssoStore.Load().ClientSecret
	}
	stored.ClientSecret, stored.AutoProvision = "", true
	if got := put(stored); got != "top-secret" {
		t.Fatalf("blank secret for the same destination stored %q", got)
	}
	stored.IssuerURL = "https://elsewhere.example.test"
	if got := put(stored); got != "" {
		t.Fatalf("stored secret followed a changed issuer: %q", got)
	}
}
