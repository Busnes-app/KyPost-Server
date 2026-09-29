package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signOnRequest(idToken string) *http.Request {
	body, _ := json.Marshal(map[string]string{"idToken": idToken})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/native/signon", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func deviceClaims(extra map[string]any) map[string]any {
	c := map[string]any{"sub": "sso-sub-12345", "preferred_username": "alice", "signon_method": "device", "jti": "jti-1", "iat": time.Now().Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func TestNativeSignOnReturnsPairingDeepLink(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = "pairing-secret"
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
	if _, err := srv.users.GetByUsername("alice"); err != nil {
		t.Fatalf("auto-provisioned user missing: %v", err)
	}

	// replay: same token, same jti
	rec = httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replay status %d", rec.Code)
	}
}

func TestNativeSignOnRefusals(t *testing.T) {
	cases := map[string]struct {
		claims map[string]any
		setup  func(*Server)
		want   int
	}{
		"webTokenRefused":     {claims: deviceClaims(map[string]any{"signon_method": nil}), want: http.StatusForbidden},
		"staleIat":            {claims: deviceClaims(map[string]any{"iat": time.Now().Add(-10 * time.Minute).Unix(), "jti": "jti-stale"}), want: http.StatusForbidden},
		"missingJti":          {claims: deviceClaims(map[string]any{"jti": nil}), want: http.StatusForbidden},
		"ssoOff":              {claims: deviceClaims(nil), setup: func(s *Server) { st := s.ssoStore.Load(); st.Enabled = false; _ = s.ssoStore.Save(st) }, want: http.StatusServiceUnavailable},
		"pairingUnconfigured": {claims: deviceClaims(nil), setup: func(s *Server) { s.pairingSecret = "" }, want: http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, idp := setupSSOTestServer(t)
			srv.pairingSecret = "pairing-secret"
			if tc.setup != nil {
				tc.setup(srv)
			}
			for k, v := range tc.claims {
				if v == nil {
					delete(tc.claims, k)
				}
			}
			idp.SetClaims(tc.claims)
			rec := httptest.NewRecorder()
			srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestNativeSignOnDirectoryDisabled(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	const sub = "sso-sub-12345"
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser(sub, "alice", true))); got != "applied" {
		t.Fatalf("create: %q", got)
	}
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-2", 2, scimUser(sub, "alice", false))); got != "applied" {
		t.Fatalf("disable: %q", got)
	}
	idp.SetClaims(deviceClaims(nil))
	rec := httptest.NewRecorder()
	srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
