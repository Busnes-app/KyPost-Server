package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Action-bound re-authentication for SSO sessions.
//
// An SSO session has no account password to re-enter, so the step-ups that
// ask for one (the Security page gate, the backup routes) send it to KySignOn
// instead: the server mints a challenge bound to the exact request that was
// attempted, the browser proves a fresh ordinary login in a popup, and the
// request is replayed carrying the challenge. The grant is spent once, only
// by the session that minted it, and only for that same action.

const (
	// ssoStepUpTTL bounds a challenge. The popup round trip to the provider
	// has to fit inside it; it matches the state cookie's own life.
	ssoStepUpTTL = 5 * time.Minute
	// ssoStepUpGrantTTL is how long a verified challenge waits to be spent.
	ssoStepUpGrantTTL = time.Minute
	// stepUpHeader carries a verified challenge back on the replayed request.
	stepUpHeader = "X-Kypost-Step-Up"
	// maxActionBytes bounds a bound request body; the gated routes read no more.
	maxActionBytes = 64 << 10
	// ssoModeStepUp marks a provider round trip that proves one action
	// rather than signing anyone in.
	ssoModeStepUp = "stepup"
)

type ssoStepUp struct {
	session  string
	action   string
	created  time.Time
	expires  time.Time
	started  bool
	verified bool
}

type actionDigestKey struct{}

// withActionDigest binds the request to itself: a digest of method, URI,
// content type and body that a step-up grant is later checked against, so a
// confirmation earned for one action can never be spent on another. The
// body is read once here and handed back for the handler to decode.
func withActionDigest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBytes))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h := sha256.New()
		h.Write([]byte(r.Method + "\x00" + r.URL.RequestURI() + "\x00" + r.Header.Get("Content-Type") + "\x00"))
		h.Write(body)
		next(w, r.WithContext(context.WithValue(r.Context(), actionDigestKey{}, hex.EncodeToString(h.Sum(nil)))))
	}
}

// sessionOf returns the live session behind the request's cookie and its token.
func (s *Server) sessionOf(r *http.Request) (Session, string, bool) {
	c, err := r.Cookie("kypost_session")
	if err != nil || c.Value == "" {
		return Session{}, "", false
	}
	s.sessMu.RLock()
	sess, ok := s.sessions[c.Value]
	s.sessMu.RUnlock()
	return sess, c.Value, ok
}

// confirmActor is the step-up a sensitive action takes: a fresh action-bound
// KySignOn authorization for a session KySignOn signed in, the account
// credential for every other session. A session from a generic provider is
// deliberately on the credential side: FreshProof speaks KySignOn's
// assurance vocabulary and nothing else, so sending such a session to its
// provider would refuse it forever, while the credential check fails closed
// on its own for an account that has none. It writes the response and
// returns false when the caller may not proceed.
func (s *Server) confirmActor(w http.ResponseWriter, r *http.Request, userID, password, authSecret string) bool {
	sess, token, ok := s.sessionOf(r)
	if !ok || !sess.SSOKySignOn {
		return s.confirmAccountCredential(w, r, userID, password, authSecret)
	}
	return s.confirmSSOStepUp(w, r, token)
}

// confirmSSOStepUp spends the verified grant the request names, or mints a
// challenge and answers 403 so the client can go and earn one.
func (s *Server) confirmSSOStepUp(w http.ResponseWriter, r *http.Request, sessionToken string) bool {
	action, _ := r.Context().Value(actionDigestKey{}).(string)
	if action == "" {
		// The route forgot withActionDigest; refusing is the safe answer.
		http.Error(w, "this action cannot be confirmed", http.StatusInternalServerError)
		return false
	}
	now := time.Now()
	if id := r.Header.Get(stepUpHeader); id != "" {
		s.stepUpMu.Lock()
		c, ok := s.stepUps[id]
		if ok = ok && c.session == sessionToken && c.action == action && c.verified && now.Before(c.expires); ok {
			delete(s.stepUps, id)
		}
		s.stepUpMu.Unlock()
		if !ok {
			http.Error(w, "the KySignOn confirmation is expired, spent or for a different action; try again", http.StatusForbidden)
			return false
		}
		s.logger.Info("SSO step-up consumed", "challenge_id", id)
		return true
	}
	id, err := sso.RandomToken(16)
	if err != nil {
		http.Error(w, "reauthentication unavailable", http.StatusInternalServerError)
		return false
	}
	id = "rea_" + id
	s.stepUpMu.Lock()
	// One live challenge per session, and nothing expired kept around.
	for k, c := range s.stepUps {
		if !now.Before(c.expires) || c.session == sessionToken {
			delete(s.stepUps, k)
		}
	}
	s.stepUps[id] = ssoStepUp{session: sessionToken, action: action, created: now, expires: now.Add(ssoStepUpTTL)}
	s.stepUpMu.Unlock()
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":     "sso_step_up_required",
		"message":   "Confirm this action with KySignOn",
		"challenge": id,
	})
	return false
}

// handleSSOStepUpStart claims a challenge once and sends the caller to the
// provider for a fresh login bound to it. A cancelled or already started
// challenge cannot be started again; the action must be retried from the top.
func (s *Server) handleSSOStepUpStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess, token, ok := s.sessionOf(r)
	if !ok || !sess.SSOKySignOn {
		http.Error(w, "only a session signed in through KySignOn can confirm with it", http.StatusBadRequest)
		return
	}
	settings := s.ssoStore.Load()
	if !settings.Enabled || settings.IssuerURL == "" || settings.ClientID == "" {
		http.Error(w, "Single Sign-On is not configured or disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || req.Challenge == "" || len(req.Challenge) > 128 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	now := time.Now()
	s.stepUpMu.Lock()
	c, ok := s.stepUps[req.Challenge]
	if ok = ok && c.session == token && !c.started && now.Before(c.expires); ok {
		c.started = true
		s.stepUps[req.Challenge] = c
	}
	s.stepUpMu.Unlock()
	if !ok {
		http.Error(w, "unknown or already started confirmation; restart the action", http.StatusForbidden)
		return
	}
	authorizeURL, _, ok := s.startSSOFlow(w, r, settings, ssoModeStepUp+":"+req.Challenge, true)
	if !ok {
		return // startSSOFlow wrote the response
	}
	s.logger.Info("SSO step-up started", "challenge_id", req.Challenge)
	writeJSON(w, http.StatusOK, map[string]any{"authorizeUrl": authorizeURL})
}

// handleSSOStepUpStatus tells the waiting page whether its challenge is
// verified yet. A challenge another session minted does not exist here.
func (s *Server) handleSSOStepUpStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, token, _ := s.sessionOf(r)
	id := r.PathValue("id")
	s.stepUpMu.Lock()
	c, ok := s.stepUps[id]
	ok = ok && c.session == token && time.Now().Before(c.expires)
	s.stepUpMu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired confirmation", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": c.verified})
}

// handleSSOStepUpCancel drops a challenge the user gave up on. Only an
// actual cancellation is logged; a stale id is not an event.
func (s *Server) handleSSOStepUpCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, token, _ := s.sessionOf(r)
	id := r.PathValue("id")
	s.stepUpMu.Lock()
	c, ok := s.stepUps[id]
	if ok = ok && c.session == token; ok {
		delete(s.stepUps, id)
	}
	s.stepUpMu.Unlock()
	if ok {
		s.logger.Info("SSO step-up cancelled", "challenge_id", id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// completeSSOStepUp is the callback's answer to a step-up round trip: it
// verifies the token as the same identity the session was minted for, with
// authentication fresh since the challenge was minted, and marks the
// challenge verified. It signs nobody in. A failed proof discards the
// challenge, so the action has to be restarted rather than retried.
func (s *Server) completeSSOStepUp(w http.ResponseWriter, r *http.Request, id string, claims *sso.SSOTokenClaims) {
	w.Header().Set("Cache-Control", "no-store")
	sess, token, ok := s.sessionOf(r)
	if !ok {
		http.Error(w, "sign in before confirming an action", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	s.stepUpMu.Lock()
	c, ok := s.stepUps[id]
	mine := ok && c.session == token
	proven := mine && c.started && !c.verified && now.Before(c.expires) &&
		claims.Issuer == sess.SSO.Issuer && claims.Sub == sess.SSO.Subject &&
		claims.FreshProof(c.created, now)
	switch {
	case proven:
		c.verified = true
		if grant := now.Add(ssoStepUpGrantTTL); grant.Before(c.expires) {
			c.expires = grant
		}
		s.stepUps[id] = c
	case mine:
		delete(s.stepUps, id)
	}
	s.stepUpMu.Unlock()
	if !proven {
		http.Error(w, "fresh KySignOn authentication was not proven for this session; restart the action", http.StatusForbidden)
		return
	}
	s.logger.Info("SSO step-up verified", "challenge_id", id)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><html lang=en><meta charset=utf-8><title>KyPost confirmation</title><p>Identity confirmed. Return to KyPost to finish the action.</p></html>")
}
