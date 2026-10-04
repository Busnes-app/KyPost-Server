package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailRelayAdminAuthorityRedactionAndRotation(t *testing.T) {
	srv := newDirectoryTestServer(t)
	const password = "long-password-for-relay-admin"
	admin, err := srv.users.Create(context.Background(), "relay-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(context.Background(), "relay-member", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	memberToken, memberCSRF := mintSessionForTest(srv, member.ID)
	call := func(method, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/mail-relay", strings.NewReader(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		}
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	for _, c := range []struct {
		method, token, csrf, body string
		want                      int
	}{
		{"GET", "", "", "", 401},
		{"GET", memberToken, memberCSRF, "", 403},
		{"PUT", "", "", "{}", 401},
		{"PUT", memberToken, memberCSRF, "{}", 403},
		{"PUT", token, "", "{}", 403},
		{"PUT", token, csrf, "{}", 401},
	} {
		w := call(c.method, c.token, c.csrf, c.body)
		if w.Code != c.want {
			t.Fatalf("privilege/CSRF/credential gate: got %d want %d: %s", w.Code, c.want, w.Body)
		}
	}
	body, err := json.Marshal(map[string]any{"host": "smtp.example.test", "smtpUsername": "operator-relay-login", "smtpPassword": "operator-relay-secret", "password": password})
	if err != nil {
		t.Fatal(err)
	}
	if w := call("PUT", token, csrf, string(body)); w.Code != 409 {
		t.Fatal("unproven domain accepted relay", w.Code, w.Body)
	}
	path, keyPath := filepath.Join(srv.configDir, "native-relay.json"), filepath.Join(srv.configDir, "native-relay.key")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("refusal created a relay key", err)
	}
	if _, err := srv.nativeDomains.Configure(context.Background(), "example.test", srv.ssoStore.Load().IssuerURL); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	srv.nativeDomains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		lookups++
		d, err := srv.nativeDomains.Read()
		if name != d.RecordName()+"." {
			t.Fatal("non-absolute domain lookup", name)
		}
		return []string{d.RecordValue()}, err
	})
	firstGeneration := ""
	for i := 0; i < 2; i++ {
		w := call("PUT", token, csrf, string(body))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "operator-relay") || strings.Contains(w.Body.String(), password) {
			t.Fatal("relay write/redaction", w.Code, w.Body)
		}
		var response struct {
			Configured, SendingEnabled bool
			Generation                 string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.Configured || response.SendingEnabled || response.Generation == "" {
			t.Fatal("configuration reported transport readiness", response, err)
		}
		if i == 0 {
			firstGeneration = response.Generation
		} else if response.Generation == firstGeneration {
			t.Fatal("relay rotation reused generation")
		}
	}
	if lookups != 2 {
		t.Fatal("rotation reused cached domain proof", lookups)
	}
	w := call("GET", token, csrf, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "operator-relay") || strings.Contains(w.Body.String(), "smtpPassword") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("relay read leaked authentication", w.Code, w.Body)
	}
	saved, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
	if err != nil || !exists || saved.Username != "operator-relay-login" || saved.Password != "operator-relay-secret" || saved.Port != 465 {
		t.Fatal("stored relay/default implicit TLS port", exists, err)
	}
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, failLookup := range []func(context.Context, string) ([]string, error){
		func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") },
		func(context.Context, string) ([]string, error) {
			d, err := srv.nativeDomains.Read()
			if err != nil {
				return nil, err
			}
			_, err = srv.nativeDomains.Configure(context.Background(), d.Domain, d.Issuer)
			return []string{d.RecordValue()}, err
		},
		func(context.Context, string) ([]string, error) {
			d, err := srv.nativeDomains.Read()
			if err != nil {
				return nil, err
			}
			err = os.WriteFile(filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile), []byte("hold during DNS lookup"), 0600)
			return []string{d.RecordValue()}, err
		},
	} {
		srv.nativeDomains.SetLookupForTest(failLookup)
		if w := call("PUT", token, csrf, string(body)); w.Code != 409 {
			t.Fatal("stale/failed proof accepted", w.Code, w.Body)
		}
		now, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(prior, now) {
			t.Fatal("failed proof changed relay", err)
		}
	}
	if err := os.WriteFile(filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile), []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := call("PUT", token, csrf, string(body)); w.Code != 409 {
		t.Fatal("held restore configured relay", w.Code, w.Body)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", token, csrf, ""); w.Code != 503 {
		t.Fatal("missing key not refused", w.Code, w.Body)
	}
}

func TestNativeMailRelayKySignOnRequiresBoundStepUp(t *testing.T) {
	srv := newDirectoryTestServer(t)
	admin, err := srv.users.CreateSSOUser("relay-sso-admin", users.RoleAdmin, "relay-admin-sub", "", "")
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	srv.sessMu.Lock()
	session := srv.sessions[token]
	session.SSOKySignOn, session.SSOAppAdmin = true, true
	session.SSO = sso.SessionIdentity{Issuer: srv.ssoStore.Load().IssuerURL, Subject: "relay-admin-sub"}
	srv.sessions[token] = session
	srv.sessMu.Unlock()
	req := httptest.NewRequest("PUT", "/api/admin/mail-relay", strings.NewReader(`{"host":"smtp.example.test","smtpUsername":"login","smtpPassword":"secret"}`))
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	challengeFrom(t, w)
	if _, err := os.Stat(filepath.Join(srv.configDir, "native-relay.json")); !os.IsNotExist(err) {
		t.Fatal("step-up refusal wrote relay", err)
	}
}
