package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/backup"
	"github.com/Busness-app/kypost-server/backend/internal/config"
	"github.com/Busness-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

func csrfFor(srv *Server, c *http.Cookie) string {
	srv.sessMu.RLock()
	defer srv.sessMu.RUnlock()
	return srv.sessions[c.Value].CSRFToken
}

// gatedCall sends a cookie-authenticated JSON request through the real
// router, carrying a step-up grant when one is given.
func gatedCall(t *testing.T, srv *Server, c *http.Cookie, method, path, body, grant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = ssoTestHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfFor(srv, c))
	req.AddCookie(c)
	if grant != "" {
		req.Header.Set(stepUpHeader, grant)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func challengeFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with a challenge: %s", rec.Code, rec.Body.String())
	}
	var body struct{ Error, Challenge string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "sso_step_up_required" || body.Challenge == "" {
		t.Fatalf("refusal did not carry a challenge: %s", rec.Body.String())
	}
	return body.Challenge
}

func stepUpStatus(t *testing.T, srv *Server, c *http.Cookie, id string) (int, bool) {
	t.Helper()
	rec := gatedCall(t, srv, c, http.MethodGet, "/api/auth/oidc/step-up/"+id, "", "")
	var body struct{ Verified bool }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.Verified
}

// freshClaims is what KySignOn answers a prompt=login round trip with.
func freshClaims(sub, sid string) map[string]any {
	return map[string]any{
		"sub": sub, "preferred_username": sub, "sid": sid,
		"auth_time": time.Now().Unix(),
		"acr":       "urn:kysignon:acr:password",
		"amr":       []string{"pwd"},
	}
}

// proveStepUp starts the provider round trip for a challenge and completes it
// with the given token claims, returning the callback's response.
func proveStepUp(t *testing.T, srv *Server, idp *ssotest.IdP, c *http.Cookie, challenge string, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	start := gatedCall(t, srv, c, http.MethodPost, "/api/auth/oidc/step-up", `{"challenge":"`+challenge+`"}`, "")
	if start.Code != http.StatusOK {
		t.Fatalf("step-up start: status = %d: %s", start.Code, start.Body.String())
	}
	for _, want := range []string{"prompt=login", "max_age=0", "acr_values=urn%3Akysignon%3Aacr%3Apassword"} {
		if !strings.Contains(start.Body.String(), want) {
			t.Fatalf("authorize URL lacks %s: %s", want, start.Body.String())
		}
	}
	idp.SetClaims(claims)
	return redeemSSOLink(t, srv, idp, c, start)
}

func TestSSOStepUpConfirmsTheSecurityPageGate(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-1")

	challenge := challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", ""))
	if code, verified := stepUpStatus(t, srv, cookie, challenge); code != http.StatusOK || verified {
		t.Fatalf("fresh challenge: status %d verified %v, want 200 and false", code, verified)
	}

	rec := proveStepUp(t, srv, idp, cookie, challenge, freshClaims("alice", "sid-2"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Identity confirmed") || sessionCookieFrom(rec) != nil {
		t.Fatalf("proof callback: status %d, cookie %v: %s", rec.Code, sessionCookieFrom(rec), rec.Body.String())
	}
	if _, verified := stepUpStatus(t, srv, cookie, challenge); !verified {
		t.Fatal("challenge is not verified after the proof")
	}
	if !sessionAlive(srv, cookie) {
		t.Fatal("the proof round trip ended the session it was proving")
	}

	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", challenge); rec.Code != http.StatusOK {
		t.Fatalf("replay with the grant: status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", challenge); rec.Code != http.StatusForbidden {
		t.Fatalf("a spent grant was accepted again: %d", rec.Code)
	}
}

func TestSSOStepUpIsBoundToTheActionAndTheSession(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	alice := ssoSignIn(t, srv, idp, "alice", "sid-a")
	bob := ssoSignIn(t, srv, idp, "bob", "sid-b")

	challenge := challengeFrom(t, gatedCall(t, srv, alice, http.MethodPost, "/api/auth/step-up", "{}", ""))

	// Bob cannot see, start or cancel Alice's challenge.
	if code, _ := stepUpStatus(t, srv, bob, challenge); code != http.StatusNotFound {
		t.Fatalf("another session sees the challenge: %d", code)
	}
	if rec := gatedCall(t, srv, bob, http.MethodPost, "/api/auth/oidc/step-up", `{"challenge":"`+challenge+`"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("another session started the challenge: %d", rec.Code)
	}
	gatedCall(t, srv, bob, http.MethodDelete, "/api/auth/oidc/step-up/"+challenge, "", "")
	if code, _ := stepUpStatus(t, srv, alice, challenge); code != http.StatusOK {
		t.Fatal("another session's cancel removed the challenge")
	}

	if rec := proveStepUp(t, srv, idp, alice, challenge, freshClaims("alice", "sid-a2")); rec.Code != http.StatusOK {
		t.Fatalf("proof: %d %s", rec.Code, rec.Body.String())
	}
	// A verified grant is for the exact request that was refused, not for
	// the route.
	if rec := gatedCall(t, srv, alice, http.MethodPost, "/api/auth/step-up", `{"other":true}`, challenge); rec.Code != http.StatusForbidden {
		t.Fatalf("grant accepted for a different body: %d", rec.Code)
	}
	if rec := gatedCall(t, srv, alice, http.MethodPost, "/api/auth/step-up", "{}", challenge); rec.Code != http.StatusOK {
		t.Fatalf("grant refused for its own action after a mismatch: %d %s", rec.Code, rec.Body.String())
	}

	// Starting a challenge twice is refused: the popup was already sent.
	again := challengeFrom(t, gatedCall(t, srv, alice, http.MethodPost, "/api/auth/step-up", "{}", ""))
	if rec := gatedCall(t, srv, alice, http.MethodPost, "/api/auth/oidc/step-up", `{"challenge":"`+again+`"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("first start: %d", rec.Code)
	}
	if rec := gatedCall(t, srv, alice, http.MethodPost, "/api/auth/oidc/step-up", `{"challenge":"`+again+`"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("second start of the same challenge: %d, want 403", rec.Code)
	}
	// Cancelling drops it.
	gatedCall(t, srv, alice, http.MethodDelete, "/api/auth/oidc/step-up/"+again, "", "")
	if code, _ := stepUpStatus(t, srv, alice, again); code != http.StatusNotFound {
		t.Fatalf("cancelled challenge still answers: %d", code)
	}

	// A password session is untouched: it is asked for the password, never
	// sent to KySignOn.
	local, err := srv.users.Create(context.Background(), "local-p", "local-p-password-123", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	clearMustChangePassword(t, srv, local.ID)
	token, _ := mintSessionForTest(srv, local.ID)
	rec := gatedCall(t, srv, &http.Cookie{Name: "kypost_session", Value: token}, http.MethodPost, "/api/auth/step-up", `{"password":"wrong"}`, "")
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "sso_step_up_required") {
		t.Fatalf("password session: status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSSOStepUpRefusesStaleOrWeakProof(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-1")

	cases := []struct {
		name   string
		claims func() map[string]any
	}{
		{"auth_time before the challenge", func() map[string]any {
			c := freshClaims("alice", "s")
			c["auth_time"] = time.Now().Add(-10 * time.Minute).Unix()
			return c
		}},
		{"no auth_time", func() map[string]any {
			c := freshClaims("alice", "s")
			delete(c, "auth_time")
			return c
		}},
		{"recovery code sign-in", func() map[string]any {
			c := freshClaims("alice", "s")
			c["amr"] = []string{"pwd", "urn:kysignon:amr:recovery"}
			return c
		}},
		{"mfa assurance without a second factor", func() map[string]any {
			c := freshClaims("alice", "s")
			c["acr"], c["amr"] = "urn:kysignon:acr:mfa", []string{"pwd", "mfa"}
			return c
		}},
		{"unknown assurance", func() map[string]any {
			c := freshClaims("alice", "s")
			c["acr"] = "urn:kysignon:acr:something"
			return c
		}},
		{"another subject", func() map[string]any { return freshClaims("mallory", "s") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			challenge := challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", ""))
			if rec := proveStepUp(t, srv, idp, cookie, challenge, tc.claims()); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
				t.Fatalf("status = %d, want 403 and no cookie: %s", rec.Code, rec.Body.String())
			}
			// A failed proof discards the challenge; the action restarts.
			if code, _ := stepUpStatus(t, srv, cookie, challenge); code != http.StatusNotFound {
				t.Fatalf("challenge survived a failed proof: %d", code)
			}
			if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", challenge); rec.Code != http.StatusForbidden {
				t.Fatalf("an unproven challenge was spent: %d", rec.Code)
			}
		})
	}
	// MFA assurance with a real second factor is fine.
	challenge := challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/auth/step-up", "{}", ""))
	c := freshClaims("alice", "s")
	c["acr"], c["amr"] = "urn:kysignon:acr:mfa", []string{"pwd", "mfa", "otp"}
	if rec := proveStepUp(t, srv, idp, cookie, challenge, c); rec.Code != http.StatusOK {
		t.Fatalf("mfa proof: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSSOStepUpGatesBackupRoutesForSSOAdmins(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.backup, _ = backup.New(backup.Dirs{Config: srv.configDir, State: srv.stateDir, Secret: srv.configDir}, config.BackupConfig{Keep: 1}, srv.globalStore, "test")
	admin := signInWith(t, srv, idp, map[string]any{"sub": "root", "preferred_username": "root", "roles": []string{"kypost.admin"}})

	const body = `{"intervalSec":3600}`
	challenge := challengeFrom(t, gatedCall(t, srv, admin, http.MethodPut, "/api/admin/backup/schedule", body, ""))
	if rec := proveStepUp(t, srv, idp, admin, challenge, freshClaims("root", "sid-2")); rec.Code != http.StatusOK {
		t.Fatalf("proof: %d %s", rec.Code, rec.Body.String())
	}
	if rec := gatedCall(t, srv, admin, http.MethodPut, "/api/admin/backup/schedule", body, challenge); rec.Code != http.StatusOK {
		t.Fatalf("schedule with the grant: %d %s", rec.Code, rec.Body.String())
	}
	if rec := gatedCall(t, srv, admin, http.MethodPut, "/api/admin/backup/schedule", body, challenge); rec.Code != http.StatusForbidden {
		t.Fatalf("schedule with a spent grant: %d", rec.Code)
	}
}
