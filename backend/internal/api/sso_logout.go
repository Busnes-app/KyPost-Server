package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
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
	limited, gen := s.ssoMeter(w, r) // the identity provider calling, not an office sign-in
	if limited {
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
	if s.multiTenant() {
		switch s.logoutOrgStatus(r.Context(), token, settings.IssuerURL, claims.Subject) {
		case http.StatusBadRequest:
			http.Error(w, "invalid logout token", http.StatusBadRequest)
			return
		case http.StatusServiceUnavailable:
			writeUnavailable(w)
			return
		}
	}

	retain := time.Now().Add(ssoLoginLifetime)
	if claims.ReplayUntil.After(retain) {
		retain = claims.ReplayUntil
	}
	// End the covered sessions before the token is spent: a failure leaves it unrecorded, so
	// the provider's retry is not refused as a replay. A replayed token ends them again,
	// which only repeats a logout.
	event := sso.NewLogoutEvent(settings.ClientID, claims, retain)
	revoked, err := s.revokeSessionsCoveredBy(event)
	if err != nil {
		s.logger.Error("SSO logout not applied: its sessions could not be ended", "error", err.Error())
		writeUnavailable(w)
		return
	}
	_, fresh, err := s.ssoLifecycle.RecordLogout(settings.ClientID, claims, retain)
	if err != nil {
		s.logger.Error("SSO logout was not recorded", "error", err.Error())
		http.Error(w, "logout was not committed", http.StatusInternalServerError)
		return
	}
	if !fresh {
		http.Error(w, "logout token was already used", http.StatusBadRequest)
		return
	}
	// Once more after recording, for a sign-in that minted a covered session in between
	// (recorded logouts fence later sign-ins). A failure here is finished by the retry,
	// whose first pass above ends it.
	again, err := s.revokeSessionsCoveredBy(event)
	if err != nil {
		s.logger.Error("SSO logout recorded but a racing session could not be ended", "error", err.Error())
		writeUnavailable(w)
		return
	}
	revoked += again
	if s.loginParamsLimiter != nil {
		s.loginParamsLimiter.refund(lockoutKeyForIP(clientIP(r)), gen)
	}
	s.logger.Info("SSO back-channel logout applied", "jti", claims.JWTID, "sessions", strconv.Itoa(revoked))
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

// endSSOSessions deletes every SSO session and bumps ssoEpoch under the same
// lock, so a callback already past its settings read cannot mint one after.
func (s *Server) endSSOSessions() error {
	if s.ssoSessionsEndFault != nil {
		if err := s.ssoSessionsEndFault(); err != nil {
			return err
		}
	}
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		return s.shared.EndSSOSessions(ctx)
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	s.ssoEpoch.Add(1)
	for token, sess := range s.sessions {
		if sess.SSO.Subject != "" {
			delete(s.sessions, token)
		}
	}
	return nil
}

// revokeSessionsCoveredBy deletes every session the logout addresses and
// returns how many. Scope is sso.LogoutEvent.Covers: a session by its sid,
// or a subject's sessions issued no later than the token.
func (s *Server) revokeSessionsCoveredBy(event sso.LogoutEvent) (int, error) {
	if s.ssoSessionsEndFault != nil {
		if err := s.ssoSessionsEndFault(); err != nil {
			return 0, err
		}
	}
	if s.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		return s.shared.DeleteSSOSessionsWhere(ctx, func(data string) bool {
			var sess Session
			return json.Unmarshal([]byte(data), &sess) != nil || event.Covers(sess.SSO)
		})
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	n := 0
	for token, sess := range s.sessions {
		if sess.SSO.Subject != "" && event.Covers(sess.SSO) {
			delete(s.sessions, token)
			n++
		}
	}
	return n, nil
}

// logoutOrgStatus is the multi-mode org check on a verified logout token, before any
// session is touched: it needs a valid org_id and a sub, so a sid alone never reaches
// another org's session, and the sub's account must be bound to that org. 0 admits.
func (s *Server) logoutOrgStatus(ctx context.Context, token, issuer, subject string) int {
	org := sso.LogoutOrg(token)
	if !config.ValidOrgID(org) || subject == "" {
		return http.StatusBadRequest
	}
	u, err := s.users.GetBySSOSubIssuer(issuer, subject)
	if errors.Is(err, users.ErrNotFound) || errors.Is(err, users.ErrNativeAccountConflict) {
		return 0 // no account under this issuer: no session to end, as today
	}
	if err == nil {
		err = s.checkAccountOrg(ctx, u, org)
	}
	switch {
	case errors.Is(err, tenancy.ErrOrgMismatch):
		s.orgRefused("backchannel_logout", u.ID)
		return http.StatusBadRequest
	case err != nil:
		return http.StatusServiceUnavailable
	}
	return 0
}
