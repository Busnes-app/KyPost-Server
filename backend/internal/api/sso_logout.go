package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/sso"
)

// ssoLoginLifetime is the longest a login can take between leaving for the
// provider and minting a session here: the state cookie's five minutes. A
// logout record is retained at least this long past its own token expiry so
// a callback that was in flight when the logout landed still hits the fence.
const ssoLoginLifetime = 5 * time.Minute

// maxLogoutTokenBytes bounds the form body. A logout token is a few hundred
// bytes of RS256 JWT.
const maxLogoutTokenBytes = 64 << 10

// handleSSOBackchannelLogout receives an OpenID Connect back-channel logout
// token from the provider and ends the sessions it names.
//
// The token is the credential: the route is public, and the only thing that
// makes a request worth acting on is a signature from the issuer's JWKS on a
// `logout+jwt` addressed to this client. A verified token is then admitted
// exactly once, durably, before any session is touched, so a replay after
// restart revokes nothing and a crash between the two leaves nothing live —
// sessions are in memory and die with the process.
//
// An acknowledgement means the token was accepted, not that a session
// existed: a logout for a session this server never saw is a success.
//
// The per-IP limiter is charged before discovery, which is an outbound fetch
// to the operator's provider, and refunded once the delivery is accepted: a
// burst of real logouts is never throttled, junk pays for itself.
func (s *Server) handleSSOBackchannelLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	settings := s.ssoStore.Load()
	if !settings.Enabled || settings.IssuerURL == "" || settings.ClientID == "" {
		http.Error(w, "Single Sign-On is not configured or disabled", http.StatusServiceUnavailable)
		return
	}
	if s.ssoRateLimited(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLogoutTokenBytes)
	if err := r.ParseForm(); err != nil || len(r.PostForm["logout_token"]) != 1 {
		http.Error(w, "expected exactly one logout_token form field", http.StatusBadRequest)
		return
	}
	token := r.PostForm["logout_token"][0]

	provider, _, err := s.ssoProvider(r, settings)
	if err != nil {
		s.ssoFailure(w, "discovery", err)
		return
	}

	claims, err := provider.VerifyLogout(r.Context(), token)
	if err != nil {
		http.Error(w, "invalid logout token", http.StatusBadRequest)
		return
	}

	retain := time.Now().Add(ssoLoginLifetime)
	if claims.ReplayUntil.After(retain) {
		retain = claims.ReplayUntil
	}
	event, fresh, err := s.ssoLifecycle.RecordLogout(settings.ClientID, claims, retain)
	if err != nil {
		s.logger.Error("SSO logout was not recorded", "error", err.Error())
		http.Error(w, "logout was not committed", http.StatusInternalServerError)
		return
	}
	if !fresh {
		http.Error(w, "logout token was already used", http.StatusBadRequest)
		return
	}

	revoked := s.revokeSessionsCoveredBy(event)
	if s.loginParamsLimiter != nil {
		s.loginParamsLimiter.settleCost(lockoutKeyForIP(clientIP(r)), -1)
	}
	s.logger.Info("SSO back-channel logout applied", "jti", claims.JWTID, "sessions", strconv.Itoa(revoked))
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

// revokeSessionsCoveredBy deletes every session the logout addresses and
// returns how many. Scope is sso.LogoutEvent.Covers: a session by its sid,
// or a subject's sessions issued no later than the token.
func (s *Server) revokeSessionsCoveredBy(event sso.LogoutEvent) int {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	n := 0
	for token, sess := range s.sessions {
		if sess.SSO.Subject != "" && event.Covers(sess.SSO) {
			delete(s.sessions, token)
			n++
		}
	}
	return n
}
