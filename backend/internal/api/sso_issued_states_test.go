package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A sign-in pays once: with a single token, the login route spends it and the
// callback still completes. The next start is refused with a Retry-After.
func TestSSOSignInIsChargedOnce(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.loginParamsLimiter = newKeyedLimiter(1, 0)

	if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusFound {
		t.Fatalf("callback answered %d, want 302: charged a second time", rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/login", nil)
	req.Host = ssoTestHost
	srv.handleSSOLogin(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("spent start answered %d with Retry-After %q, want 429 with one", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// The state cookie is the client's to write, so a state the login route never
// issued still pays at the callback.
func TestSSOCallbackChargesAnUnissuedState(t *testing.T) {
	srv, _ := setupSSOTestServer(t)
	srv.loginParamsLimiter = newKeyedLimiter(0, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=c&state=forged", nil)
	req.Host = ssoTestHost
	req.AddCookie(&http.Cookie{Name: ssoCookieName, Value: "forged|verifier|nonce|login"})
	srv.handleSSOCallback(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("forged state answered %d, want 429", rec.Code)
	}
}

func TestIssuedSSOStatesAreSingleUseAndExpire(t *testing.T) {
	var s issuedSSOStates
	now := time.Now()
	s.add("a", now)
	s.add("b", now)
	if !s.take("a", now) || s.take("a", now) {
		t.Fatal("an issued state did not redeem exactly once")
	}
	if s.take("b", now.Add(issuedSSOStateTTL+time.Second)) {
		t.Fatal("an expired state redeemed")
	}
	if s.take("never", now) {
		t.Fatal("an unissued state redeemed")
	}
}
