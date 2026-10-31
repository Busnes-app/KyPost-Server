package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// suiteAppsTTL is how long one user's app list is reused: KyIdentity rate limits each client.
var suiteAppsTTL = time.Minute

// suiteAppsEntry is one user's cached list, valid only for the directory and subject it was
// fetched with.
type suiteAppsEntry struct {
	dir   suitedir.Client
	sub   string
	apps  []suitedir.App
	until time.Time
}

// handleSuiteApps is the app rail: the suite apps KyIdentity lets the caller open.
func (s *Server) handleSuiteApps(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string][]suitedir.App{"apps": s.suiteAppsFor(r.Context(), ac.UserID)})
}

// suiteAppsFor asks KyIdentity's suite directory with the user's linked subject, using
// KyPost's own SSO client (built as kycalendar's directory pairing builds it). Accounts not
// linked to the configured issuer, and every failure, get an empty list.
func (s *Server) suiteAppsFor(ctx context.Context, userID string) []suitedir.App {
	none := []suitedir.App{}
	st := s.ssoStore.Load()
	if !st.Enabled || st.IssuerURL == "" || st.ClientID == "" || st.ClientSecret == "" {
		return none
	}
	u, err := s.users.Get(userID)
	if err != nil || !linkedTo(u, st) {
		return none
	}
	dir := suitedir.Client{Issuer: st.IssuerURL, ClientID: st.ClientID, ClientSecret: st.ClientSecret}
	// ponytail: one entry per user who opened the app, never swept; bounded by the user count.
	if v, ok := s.suiteApps.Load(userID); ok {
		if e := v.(suiteAppsEntry); e.dir == dir && e.sub == u.SSOSub && time.Now().Before(e.until) {
			return e.apps
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	apps, err := dir.Apps(tctx, u.SSOSub)
	cancel()
	if err != nil {
		// suitedir errors carry neither the request URL (the sub) nor the credentials.
		s.logger.Warn("suite app list unavailable", "user_id", userID, "error", err.Error())
		apps = none
	}
	// A request the browser abandoned says nothing about KyIdentity: do not cache it.
	if ctx.Err() == nil {
		s.suiteApps.Store(userID, suiteAppsEntry{dir: dir, sub: u.SSOSub, apps: apps, until: time.Now().Add(suiteAppsTTL)})
	}
	return apps
}

// linkedTo reports a live SSO link to the configured issuer. Stored issuers may or may not
// carry the trailing slash the settings have.
func linkedTo(u users.User, st sso.SSOSettings) bool {
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	return u.SSOSub != "" && !u.SSOLinkRevoked() && trim(u.SSOIssuer) == trim(st.IssuerURL)
}
