package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func signOnRequest(idToken string) *http.Request {
	body, _ := json.Marshal(map[string]string{"idToken": idToken})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/native/signon", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// nativeSignOnServer serves https, as KyIdentity only issues https origins.
// The SPKI probe's failure cache is primed so no test dials the base URL.
func nativeSignOnServer(t *testing.T) (*Server, *ssotest.IdP) {
	t.Helper()
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = "pairing-secret"
	srv.serverBaseURL = "https://" + ssoTestHost
	srv.pinProbeHost, srv.pinProbeFailedAt = srv.serverBaseURL, time.Now()
	return srv, idp
}

// registerWithPairingToken redeems a deep link's token at native/register.
func registerWithPairingToken(t *testing.T, srv *Server, deepLink string) *httptest.ResponseRecorder {
	t.Helper()
	link, err := url.Parse(deepLink)
	if err != nil {
		t.Fatalf("deep link %q: %v", deepLink, err)
	}
	body, _ := json.Marshal(map[string]any{
		"subscriberId": link.Query().Get("sub"),
		"pairingToken": link.Query().Get("pt"),
		"deviceToken":  "native-device-token",
		"deviceId":     "device-signon",
	})
	rec := httptest.NewRecorder()
	srv.handleNotificationNativeRegister(rec, httptest.NewRequest(http.MethodPost, "/api/notifications/native/register", jsonBody(body)))
	return rec
}

func deviceClaims(extra map[string]any) map[string]any {
	c := map[string]any{"sub": "sso-sub-12345", "preferred_username": "alice", "signon_method": "device", "origin": "https://" + ssoTestHost, "jti": "jti-1", "iat": time.Now().Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func TestNativeSignOnReturnsPairingDeepLink(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	idp.SetClaims(deviceClaims(nil))

	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		DeepLink   string `json:"deepLink"`
		Configured bool   `json:"configured"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.Configured || !strings.HasPrefix(body.DeepLink, "kypost://native-pair?") {
		t.Fatalf("body %s", rec.Body.String())
	}
	alice, err := srv.users.GetByUsername("alice")
	if err != nil {
		t.Fatalf("auto-provisioned user missing: %v", err)
	}
	store, err := srv.userStore(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantSub, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(body.DeepLink)
	if err != nil || link.Query().Get("sub") != wantSub {
		t.Fatalf("deep link sub %q, want %q (%v)", link.Query().Get("sub"), wantSub, err)
	}

	if reg := registerWithPairingToken(t, srv, body.DeepLink); reg.Code != http.StatusOK {
		t.Fatalf("register with a fresh sign-on token: %d %s", reg.Code, reg.Body.String())
	}

	// replay: same token, same jti
	rec = httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replay status %d", rec.Code)
	}
}

// The canonical origin ignores a path, so a claim carrying one is accepted.
func TestNativeSignOnOriginWithPathAccepted(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	idp.SetClaims(deviceClaims(map[string]any{"origin": "https://" + ssoTestHost + "/mail/"}))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestCanonicalOrigin(t *testing.T) {
	same := [][2]string{
		{"https://Mail.Example.com/", "https://mail.example.com"},
		{"https://mail.example.com/a/b?q=1", "https://mail.example.com"},
		{"https://mail.example.com:443", "https://mail.example.com"},
		{"https://mail.example.com:8443/x", "https://mail.example.com:8443"},
		{"http://mail.example.com:443", "http://mail.example.com:443"},
	}
	for _, c := range same {
		if got := canonicalOrigin(c[0]); got != c[1] {
			t.Errorf("canonicalOrigin(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	differ := [][2]string{
		{"https://mail.example.com", "http://mail.example.com"},
		{"https://mail.example.com:8443", "https://mail.example.com"},
		{"http://mail.example.com:443", "http://mail.example.com"},
		{"https://mail.example.com", "https://mail.example.org"},
	}
	for _, c := range differ {
		if canonicalOrigin(c[0]) == canonicalOrigin(c[1]) {
			t.Errorf("%q and %q must differ", c[0], c[1])
		}
	}
	for _, bad := range []string{"", "mail.example.com", "://x", "https://"} {
		if got := canonicalOrigin(bad); got != "" {
			t.Errorf("canonicalOrigin(%q) = %q, want empty", bad, got)
		}
	}
}

func TestNativeSignOnRefusals(t *testing.T) {
	const notDevice = "not issued for device sign-in"
	const differentServer = "issued for a different server"
	cases := map[string]struct {
		claims   map[string]any
		setup    func(*Server, *ssotest.IdP)
		body     func(idToken string) *http.Request
		want     int
		wantBody string
	}{
		"webTokenRefused":   {claims: deviceClaims(map[string]any{"signon_method": nil}), want: http.StatusForbidden, wantBody: notDevice},
		"missingJti":        {claims: deviceClaims(map[string]any{"jti": nil}), want: http.StatusForbidden, wantBody: notDevice},
		"originMissing":     {claims: deviceClaims(map[string]any{"origin": nil, "jti": "jti-om"}), want: http.StatusForbidden, wantBody: differentServer},
		"originOtherHost":   {claims: deviceClaims(map[string]any{"origin": "https://evil.example", "jti": "jti-oh"}), want: http.StatusForbidden, wantBody: differentServer},
		"originOtherPort":   {claims: deviceClaims(map[string]any{"origin": "https://" + ssoTestHost + ":8443", "jti": "jti-op"}), want: http.StatusForbidden, wantBody: differentServer},
		"originOtherScheme": {claims: deviceClaims(map[string]any{"origin": "http://" + ssoTestHost, "jti": "jti-os"}), want: http.StatusForbidden, wantBody: differentServer},
		"staleIat":          {claims: deviceClaims(map[string]any{"iat": time.Now().Add(-10 * time.Minute).Unix(), "jti": "jti-stale"}), want: http.StatusForbidden, wantBody: "too old"},
		"futureIat":         {claims: deviceClaims(map[string]any{"iat": time.Now().Add(2 * time.Minute).Unix(), "jti": "jti-future"}), want: http.StatusForbidden, wantBody: "not valid yet"},
		"wrongAudience": {claims: deviceClaims(nil), setup: func(_ *Server, idp *ssotest.IdP) { idp.WrongAudience = "someone-else" },
			want: http.StatusForbidden, wantBody: "could not be verified"},
		"ssoOff": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) { st := s.ssoStore.Load(); st.Enabled = false; _ = s.ssoStore.Save(st) },
			want: http.StatusServiceUnavailable, wantBody: "Single Sign-On is not configured"},
		"pairingUnconfigured": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) { s.pairingSecret = "" },
			want: http.StatusServiceUnavailable, wantBody: "pairing is not configured"},
		"oversizeBody": {claims: deviceClaims(nil), body: func(string) *http.Request { return signOnRequest(strings.Repeat("a", 17*1024)) },
			want: http.StatusBadRequest, wantBody: "invalid request"},
		"emptyBody": {claims: deviceClaims(nil), body: func(string) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/api/auth/native/signon", strings.NewReader(""))
		}, want: http.StatusBadRequest, wantBody: "invalid request"},
		"rateLimited": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) {
			for range loginParamsBurst + 1 {
				s.loginParamsLimiter.allow(lockoutKeyForIP(clientIP(signOnRequest("x"))))
			}
		}, want: http.StatusTooManyRequests, wantBody: "too many sign-in attempts"},
		"deactivatedUser": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) {
			if _, err := s.users.Create(context.Background(), "alice", "correct-horse-battery-staple", users.RoleUser); err != nil {
				t.Fatalf("create: %v", err)
			}
			u, _ := s.users.GetByUsername("alice")
			if err := s.users.LinkSSO(u.ID, "sso-sub-12345", "alice", ""); err != nil {
				t.Fatalf("link: %v", err)
			}
			if _, err := s.users.Deactivate(u.ID); err != nil {
				t.Fatalf("deactivate: %v", err)
			}
		}, want: http.StatusForbidden, wantBody: "account is deactivated"},
		"revokedBefore": {claims: deviceClaims(map[string]any{"iat": time.Now().Add(-time.Minute).Unix()}), setup: func(s *Server, idp *ssotest.IdP) {
			_, err := s.ssoLifecycle.ApplyDirectory(idp.URL(), syncauth.Event{ID: "ev-rev", At: time.Now()}, "sso-sub-12345", 1, "d", true, func() (bool, error) { return true, nil })
			if err != nil {
				t.Fatalf("directory: %v", err)
			}
		}, want: http.StatusForbidden, wantBody: "directory access changed"},
		"noClientID": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) { st := s.ssoStore.Load(); st.ClientID = ""; _ = s.ssoStore.Save(st) },
			want: http.StatusServiceUnavailable, wantBody: "Single Sign-On is not configured"},
		"noBaseURL": {claims: deviceClaims(nil), setup: func(s *Server, _ *ssotest.IdP) { s.serverBaseURL = "" },
			want: http.StatusServiceUnavailable, wantBody: "pairing is not configured"},
		"loggedOut": {claims: deviceClaims(map[string]any{"sid": "sid-x", "iat": time.Now().Add(-time.Minute).Unix(), "jti": "jti-lo"}),
			setup: func(s *Server, idp *ssotest.IdP) {
				if rec := postLogout(s, logoutForm(idp.LogoutToken(t, "sso-sub-12345", "sid-x", nil))); rec.Code != http.StatusOK {
					t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
				}
			}, want: http.StatusForbidden, wantBody: "ended by the identity provider"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, idp := nativeSignOnServer(t)
			if tc.setup != nil {
				tc.setup(srv, idp)
			}
			for k, v := range tc.claims {
				if v == nil {
					delete(tc.claims, k)
				}
			}
			idp.SetClaims(tc.claims)
			req := signOnRequest(idp.IDToken())
			if tc.body != nil {
				req = tc.body(idp.IDToken())
			}
			rec := httptest.NewRecorder()
			srv.handleNativeSignOn(rec, req)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("status %d want %d, body %q want substring %q", rec.Code, tc.want, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// Only the directory fence can refuse here: the subject was never provisioned,
// AutoProvision is on, and a disable event for an unknown subject records the
// fence without touching any account.
func TestNativeSignOnDirectoryDisabled(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	srv.pairingSecret = testSyncKey
	const sub = "sso-sub-12345"
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-1", 1, scimUser(sub, "alice", false))); got != "applied" {
		t.Fatalf("disable: %q", got)
	}
	if _, err := srv.users.GetBySSOSub(sub); err == nil {
		t.Fatal("the disable event provisioned an account")
	}
	idp.SetClaims(deviceClaims(nil))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "disabled by the directory") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

// A spent jti must survive a restart: a second store over the same directory
// is what the next process sees.
func TestNativeSignOnJTISurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(time.Minute)
	if fresh, err := sso.NewLifecycleStore(dir).RecordSignOnJTI("iss", "cid", "j1", exp); err != nil || !fresh {
		t.Fatalf("first spend fresh=%v err=%v", fresh, err)
	}
	if fresh, err := sso.NewLifecycleStore(dir).RecordSignOnJTI("iss", "cid", "j1", exp); err != nil || fresh {
		t.Fatalf("replay after reload fresh=%v err=%v", fresh, err)
	}
	// An expired record is pruned by the next write.
	past := time.Now().Add(-time.Minute)
	st := sso.NewLifecycleStore(dir)
	_, _ = st.RecordSignOnJTI("iss", "cid", "old", past)
	_, _ = st.RecordSignOnJTI("iss", "cid", "j2", exp)
	if fresh, _ := st.RecordSignOnJTI("iss", "cid", "old", exp); !fresh {
		t.Fatal("expired jti was not pruned")
	}
}

// An origin refusal comes before the jti is recorded, so a token for another
// server cannot burn a jti this server would accept.
func TestNativeSignOnOriginMismatchLeavesJTIUnspent(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	idp.SetClaims(deviceClaims(map[string]any{"origin": "https://evil.example", "jti": "jti-shared"}))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatch status %d body %s", rec.Code, rec.Body.String())
	}
	idp.SetClaims(deviceClaims(map[string]any{"jti": "jti-shared"}))
	rec = httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusOK {
		t.Fatalf("matching origin, same jti: status %d body %s", rec.Code, rec.Body.String())
	}
}

// linkedPasswordAccount is a password-backed account linked to the test IdP's
// subject: the shape whose link a password change revokes.
func linkedPasswordAccount(t *testing.T, srv *Server) users.User {
	t.Helper()
	u := localAccount(t, srv, "alice")
	if err := srv.users.LinkSSO(u.ID, "sso-sub-12345", "alice", ""); err != nil {
		t.Fatalf("LinkSSO: %v", err)
	}
	u, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return u
}

// Revocation completing after the fail-fast checks but before issuance must
// refuse: without the recheck under pairingMu, issuance read the rotated
// subscriber id and minted a live token.
func TestNativeSignOnRevokedBeforeIssueRefused(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	u := linkedPasswordAccount(t, srv)
	srv.nativeSignOnBeforeIssue = func() {
		if err := srv.revokeAllUserCredentials(u); err != nil {
			t.Errorf("revoke: %v", err)
		}
	}
	idp.SetClaims(deviceClaims(nil))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "pairingToken") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

// Revocation racing issuance lands either before the locked recheck (403) or
// after the subscriber read, where its rotation kills the minted token.
func TestNativeSignOnRevokedDuringIssueTokenDead(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	u := linkedPasswordAccount(t, srv)
	done := make(chan error, 1)
	srv.nativeSignOnBeforeIssue = func() {
		go func() { done <- srv.revokeAllUserCredentials(u) }()
	}
	idp.SetClaims(deviceClaims(nil))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if err := <-done; err != nil {
		t.Fatalf("revoke: %v", err)
	}
	switch rec.Code {
	case http.StatusForbidden:
	case http.StatusOK:
		var body struct {
			DeepLink string `json:"deepLink"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if reg := registerWithPairingToken(t, srv, body.DeepLink); reg.Code != http.StatusUnauthorized {
			t.Fatalf("token minted before revocation registered: %d %s", reg.Code, reg.Body.String())
		}
	default:
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
