package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// AI agent tokens authenticate /mcp and nothing else. They are a credential of
// their own, apart from the CardDAV password and device secrets: each is refused
// on the other's channel, and revokeUserCredentials removes them with the rest.
const (
	agentTokenPrefix      = "kp_"
	agentTokenIDBytes     = 8
	agentTokenSecretBytes = 32
	maxAgentTokensPerUser = 20
	// agentTokenIdleLimit is how long a token may go unused before it is revoked.
	agentTokenIdleLimit = 90 * 24 * time.Hour
)

// newAgentToken mints kp_<id>_<secret>. The secret is 32 random bytes, so a
// plain SHA-256 (users.HashDeviceSecret) is the right hash, not a password KDF.
func newAgentToken() (id, token, hash string, err error) {
	if id, err = randomToken(agentTokenIDBytes); err != nil {
		return "", "", "", err
	}
	secret, err := randomToken(agentTokenSecretBytes)
	if err != nil {
		return "", "", "", err
	}
	return id, agentTokenPrefix + id + "_" + secret, users.HashDeviceSecret(secret), nil
}

// parseAgentToken splits kp_<id>_<secret>; anything else is not an agent token.
func parseAgentToken(raw string) (id, secret string, ok bool) {
	rest, ok := strings.CutPrefix(raw, agentTokenPrefix)
	if !ok {
		return "", "", false
	}
	id, secret, ok = strings.Cut(rest, "_")
	return id, secret, ok && isLowerHex(id, 2*agentTokenIDBytes) && isLowerHex(secret, 2*agentTokenSecretBytes)
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type agentTokenView struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	// RevokeAfter is when the token is revoked unless it is used before then.
	RevokeAfter time.Time `json:"revokeAfter"`
}

func viewOfAgentToken(t state.AgentToken) agentTokenView {
	v := agentTokenView{ID: t.ID, Label: t.Label, Scope: t.Scope, CreatedAt: time.Unix(t.CreatedAt, 0).UTC(),
		RevokeAfter: time.Unix(t.LastActive(), 0).Add(agentTokenIdleLimit).UTC()}
	if t.LastUsedAt > 0 {
		at := time.Unix(t.LastUsedAt, 0).UTC()
		v.LastUsedAt = &at
	}
	return v
}

// agentTokenOwner is the signed-in everyday user managing their own tokens.
// Administrator identities have no mail, so they get no agent either.
func (s *Server) agentTokenOwner(w http.ResponseWriter, r *http.Request) (AuthContext, bool) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return AuthContext{}, false
	}
	if ac.Role == users.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "administrator accounts cannot use AI agents; use your everyday identity"})
		return AuthContext{}, false
	}
	if s.globalStore == nil {
		http.Error(w, "agent tokens are unavailable: the server state database did not open", http.StatusServiceUnavailable)
		return AuthContext{}, false
	}
	return ac, true
}

func (s *Server) handleListAgentTokens(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.agentTokenOwner(w, r)
	if !ok {
		return
	}
	list, err := s.globalStore.AgentTokens(r.Context(), ac.UserID)
	if err != nil {
		http.Error(w, "could not list AI agent tokens", http.StatusInternalServerError)
		return
	}
	out := make([]agentTokenView, 0, len(list))
	for _, t := range list {
		out = append(out, viewOfAgentToken(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// withSameOrigin refuses a browser request another site sent, on top of
// withAuth's session and CSRF checks: an Origin other than this server's own
// (an opaque "null" included), or Sec-Fetch-Site cross-site. A token minted for
// a page that is not KyPost is the whole attack (Joplin GHSA-9728-v7ww-mxjv).
func (s *Server) withSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if (origin != "" && canonicalOrigin(origin) != s.ownOrigin(r)) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-site request refused"})
			return
		}
		next(w, r)
	}
}

// ownOrigin is SERVER_BASE_URL's origin, or the one the request arrived on.
func (s *Server) ownOrigin(r *http.Request) string {
	if s.serverBaseURL != "" {
		return canonicalOrigin(s.serverBaseURL)
	}
	scheme := "http"
	if isRequestSecure(r) {
		scheme = "https"
	}
	return canonicalOrigin(scheme + "://" + r.Host)
}

const agentScopeError = "scope must be mcp:read or mcp:organize"

// handleCreateAgentToken mints a token behind step-up: it outlives the session,
// so a stolen session must not be able to mint one.
func (s *Server) handleCreateAgentToken(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.agentTokenOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		Label      string `json:"label"`
		Scope      string `json:"scope"`
		AuthSecret string `json:"authSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(req.Label)
	if n := len([]rune(label)); n == 0 || n > 64 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		http.Error(w, "label must be 1 to 64 characters", http.StatusBadRequest)
		return
	}
	// Required: no client gets an access level by default.
	if !state.IsAgentScope(req.Scope) {
		http.Error(w, agentScopeError, http.StatusBadRequest)
		return
	}
	if !s.confirmActor(w, r, ac.UserID, req.AuthSecret) {
		return
	}
	id, token, hash, err := newAgentToken()
	if err != nil {
		http.Error(w, "could not create AI agent token", http.StatusInternalServerError)
		return
	}
	t := state.AgentToken{ID: id, UserID: ac.UserID, Label: label, Hash: hash, Scope: req.Scope, CreatedAt: time.Now().Unix()}
	switch err := s.globalStore.CreateAgentToken(r.Context(), t, maxAgentTokensPerUser); {
	case errors.Is(err, state.ErrAgentTokenLimit):
		http.Error(w, "revoke an AI agent token before creating another", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "could not create AI agent token", http.StatusInternalServerError)
		return
	}
	// revokeUserCredentials ends sessions before it deletes tokens. A revocation
	// that ran since withAuth admitted this request ended this session, so a token
	// inserted after its delete is withdrawn here.
	if _, _, live := s.sessionOf(r); !live {
		_ = s.globalStore.DeleteAgentToken(r.Context(), ac.UserID, id)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	s.logAgentToken(ac.UserID, "agent_token.create", id, req.Scope)
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "agentToken": viewOfAgentToken(t)})
}

// handleSetAgentTokenScope changes a token's access level. Widening an agent's
// access is as sensitive as minting a token, so it takes step-up too.
func (s *Server) handleSetAgentTokenScope(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.agentTokenOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		Scope      string `json:"scope"`
		AuthSecret string `json:"authSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !state.IsAgentScope(req.Scope) {
		http.Error(w, agentScopeError, http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	t, err := s.globalStore.AgentToken(r.Context(), id)
	if errors.Is(err, state.ErrAgentTokenNotFound) || (err == nil && t.UserID != ac.UserID) {
		http.Error(w, "AI agent token not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not change AI agent token", http.StatusInternalServerError)
		return
	}
	if !s.confirmActor(w, r, ac.UserID, req.AuthSecret) {
		return
	}
	switch err := s.globalStore.SetAgentTokenScope(r.Context(), ac.UserID, id, req.Scope); {
	case errors.Is(err, state.ErrAgentTokenNotFound): // revoked since the read
		http.Error(w, "AI agent token not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "could not change AI agent token", http.StatusInternalServerError)
		return
	}
	if t.Scope != req.Scope {
		s.logAgentToken(ac.UserID, "agent_token.scope_change", id, t.Scope+" to "+req.Scope)
	}
	t.Scope = req.Scope
	writeJSON(w, http.StatusOK, viewOfAgentToken(t))
}

func (s *Server) handleDeleteAgentToken(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.agentTokenOwner(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	switch err := s.globalStore.DeleteAgentToken(r.Context(), ac.UserID, id); {
	case errors.Is(err, state.ErrAgentTokenNotFound):
		http.Error(w, "AI agent token not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "could not revoke AI agent token", http.StatusInternalServerError)
		return
	}
	s.logAgentToken(ac.UserID, "agent_token.revoke", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// revokeAgentTokens deletes every agent token userID holds; for paths that
// revoke tokens without revokeUserCredentials, such as a role change.
func (s *Server) revokeAgentTokens(userID string) {
	if s.globalStore == nil {
		return
	}
	if err := s.globalStore.DeleteUserAgentTokens(context.Background(), userID); err != nil {
		s.logger.Error("failed to revoke agent tokens", "user_id", userID, "error", err.Error())
	}
}

// logAgentToken records a token lifecycle event: the owner and the token ID,
// never the token.
func (s *Server) logAgentToken(userID, action, id, detail string) {
	s.logger.Info("agent token change", "actor", userID, "action", action, "target", "agent_token:"+id, "detail", detail, "result", "ok")
}

// StartAgentTokenSweeper revokes agent tokens unused for 90 days, daily.
// withMCPAuth refuses an idle token on its own; this clears the ones nobody
// presents, so they leave the token list.
func (s *Server) StartAgentTokenSweeper(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireIdleAgentTokens(ctx, time.Now())
		}
	}
}

func (s *Server) expireIdleAgentTokens(ctx context.Context, now time.Time) {
	if s.globalStore == nil {
		return
	}
	gone, err := s.globalStore.ExpireIdleAgentTokens(ctx, now.Add(-agentTokenIdleLimit).Unix(), "")
	if err != nil {
		s.logger.Error("agent token expiry failed", "error", err.Error())
	}
	for _, t := range gone {
		s.logAgentToken(t.UserID, "agent_token.expired", t.ID, "")
	}
}
