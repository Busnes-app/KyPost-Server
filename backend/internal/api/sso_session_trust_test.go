package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func putSSOSettings(t *testing.T, srv *Server, settings sso.SSOSettings) {
	t.Helper()
	body, _ := json.Marshal(settings)
	rec := httptest.NewRecorder()
	srv.handleAdminSSOPut(rec, httptest.NewRequest(http.MethodPut, "/api/admin/sso", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", rec.Code, rec.Body.String())
	}
}

func passwordSession(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	u, err := srv.users.Create(context.Background(), "local", "local-password-123", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := srv.startSession(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), u.ID); err != nil {
		t.Fatal(err)
	}
	return sessionCookieFrom(rec)
}

// A session carries the trust it was minted under: changing who is trusted
// ends every SSO session, and leaves password sessions alone.
func TestSSOTrustChangeEndsSSOSessions(t *testing.T) {
	for name, change := range map[string]func(*sso.SSOSettings){
		"issuer":      func(s *sso.SSOSettings) { s.IssuerURL = "https://other.example" },
		"clientID":    func(s *sso.SSOSettings) { s.ClientID = "other-client" },
		"adminGroup":  func(s *sso.SSOSettings) { s.AdminGroup = "kypost-admins" },
		"disabled":    func(s *sso.SSOSettings) { s.Enabled = false },
		"unchanged":   nil,
		"autoProvide": func(s *sso.SSOSettings) { s.AutoProvision = false },
	} {
		t.Run(name, func(t *testing.T) {
			srv, idp := setupSSOTestServer(t)
			ssoCookie := signInWith(t, srv, idp, map[string]any{"sub": "a", "preferred_username": "a", "roles": []string{"kypost.admin"}})
			local := passwordSession(t, srv)

			settings := srv.ssoStore.Load()
			if change != nil {
				change(&settings)
			}
			putSSOSettings(t, srv, settings)

			ends := name != "unchanged" && name != "autoProvide"
			if alive := sessionAlive(srv, ssoCookie); alive == ends {
				t.Fatalf("SSO session alive after %s change = %v, want %v", name, alive, !ends)
			}
			if !sessionAlive(srv, local) {
				t.Fatal("a password session was ended")
			}
		})
	}
}

// A callback that read the old settings cannot mint a session after the change.
func TestSSOCallbackInFlightDuringTrustChangeMintsNothing(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	settings := srv.ssoStore.Load()
	srv.ssoCallbackAfterResolve = func() {
		changed := settings
		changed.AdminGroup = "kypost-admins"
		putSSOSettings(t, srv, changed)
	}
	idp.SetClaims(map[string]any{"sub": "a", "preferred_username": "a", "roles": []string{"kypost.admin"}})
	rec := runSSOFlow(t, srv, idp, nil, false)
	if c := sessionCookieFrom(rec); c != nil && sessionAlive(srv, c) {
		t.Fatalf("callback minted a live session across a trust change: %d", rec.Code)
	}
}

// Revoking the link ends the sessions it minted, however it was revoked.
func TestRevokedSSOLinkEndsItsSessions(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	c := signInWith(t, srv, idp, map[string]any{"sub": "a", "preferred_username": "a", "roles": []string{}})
	u, err := srv.users.GetBySSOSub("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.users.RevokeSSOLink(u.ID); err != nil {
		t.Fatal(err)
	}
	if sessionAlive(srv, c) {
		t.Fatal("a session minted by a revoked link is still alive")
	}
	if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "revoked") {
		t.Fatalf("sign-in through the revoked link: %d %s", rec.Code, rec.Body.String())
	}
}
