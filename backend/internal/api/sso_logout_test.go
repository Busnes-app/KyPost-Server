package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/kypost-server/backend/internal/sso"
	"github.com/Busness-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

// ssoSignIn completes a provider login for sub under session id sid and
// returns the session cookie it minted.
func ssoSignIn(t *testing.T, srv *Server, idp *ssotest.IdP, sub, sid string) *http.Cookie {
	t.Helper()
	idp.SetClaims(map[string]any{"sub": sub, "preferred_username": "u_" + sub, "sid": sid})
	rec := runSSOFlow(t, srv, idp, nil, false)
	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in for %s/%s: status = %d (%s)", sub, sid, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatalf("sign-in for %s/%s minted no session", sub, sid)
	}
	return c
}

func postLogout(srv *Server, form url.Values) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/oidc/backchannel-logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleSSOBackchannelLogout(rec, req)
	return rec
}

func logoutForm(token string) url.Values { return url.Values{"logout_token": {token}} }

func sessionAlive(srv *Server, c *http.Cookie) bool {
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(c)
	_, ok := srv.currentUser(req)
	return ok
}

func TestBackchannelLogoutEndsOnlyTheAddressedSession(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	a := ssoSignIn(t, srv, idp, "alice", "sid-a1")
	a2 := ssoSignIn(t, srv, idp, "alice", "sid-a2")
	b := ssoSignIn(t, srv, idp, "bob", "sid-b")

	rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "alice", "sid-a1", nil)))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("logout: status = %d, Cache-Control = %q: %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	if sessionAlive(srv, a) {
		t.Error("the session the token named is still alive")
	}
	if !sessionAlive(srv, a2) || !sessionAlive(srv, b) {
		t.Error("a session the token did not name was ended")
	}

	// A session id nobody here has seen is a success, and never widens into
	// a subject-wide logout.
	if rec = postLogout(srv, logoutForm(idp.LogoutToken(t, "alice", "sid-unknown", nil))); rec.Code != http.StatusOK {
		t.Fatalf("logout for an unknown sid: status = %d: %s", rec.Code, rec.Body.String())
	}
	if !sessionAlive(srv, a2) {
		t.Error("an unknown sid ended alice's other session")
	}
}

func TestBackchannelLogoutBySubjectEndsEarlierSessionsOnly(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	a := ssoSignIn(t, srv, idp, "alice", "sid-a1")
	a2 := ssoSignIn(t, srv, idp, "alice", "sid-a2")
	b := ssoSignIn(t, srv, idp, "bob", "sid-b")
	localUser, err := srv.users.Create(context.Background(), "local-user-2", "local-user-2-testpassword", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	localToken, _ := mintSessionForTest(srv, localUser.ID)
	local := &http.Cookie{Name: "kypost_session", Value: localToken}

	// Token times are whole seconds. Step past the boundary on both sides so
	// the sessions above are older than the token and the login below newer.
	time.Sleep(1100 * time.Millisecond)
	token := idp.LogoutToken(t, "alice", "", nil)
	time.Sleep(1100 * time.Millisecond)
	if rec := postLogout(srv, logoutForm(token)); rec.Code != http.StatusOK {
		t.Fatalf("subject logout: status = %d: %s", rec.Code, rec.Body.String())
	}
	if sessionAlive(srv, a) || sessionAlive(srv, a2) {
		t.Error("a subject-wide logout left one of alice's sessions alive")
	}
	if !sessionAlive(srv, b) || !sessionAlive(srv, local) {
		t.Error("a subject-wide logout for alice ended somebody else's session")
	}

	// A login after the token is a new login and stays.
	later := ssoSignIn(t, srv, idp, "alice", "sid-a3")
	if rec := postLogout(srv, logoutForm(token)); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed token: status = %d, want 400", rec.Code)
	}
	if !sessionAlive(srv, later) {
		t.Error("a replayed subject logout ended a session minted after it")
	}
}

func TestBackchannelLogoutFencesTheLoginInFlight(t *testing.T) {
	srv, idp := setupSSOTestServer(t)

	// The provider ends session sid-c before the browser completes the login
	// for it, as happens when the user closes the tab mid-redirect.
	if rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "carol", "sid-c", nil))); rec.Code != http.StatusOK {
		t.Fatalf("logout ahead of login: status = %d: %s", rec.Code, rec.Body.String())
	}
	idp.SetClaims(map[string]any{"sub": "carol", "preferred_username": "carol", "sid": "sid-c"})
	rec := runSSOFlow(t, srv, idp, nil, false)
	if rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatalf("login for an ended session: status = %d, cookie = %v, want 403 and none", rec.Code, sessionCookieFrom(rec))
	}

	// A fresh session id for the same subject is a different login.
	ssoSignIn(t, srv, idp, "carol", "sid-c2")
}

func TestBackchannelLogoutRefusesUnverifiableTokens(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	alive := ssoSignIn(t, srv, idp, "alice", "sid-a1")

	cases := []struct {
		name string
		form func() url.Values
	}{
		{"no token", func() url.Values { return url.Values{} }},
		{"two tokens", func() url.Values {
			tok := idp.LogoutToken(t, "alice", "sid-a1", nil)
			return url.Values{"logout_token": {tok, tok}}
		}},
		{"foreign key", func() url.Values {
			idp.ForeignKey = true
			defer func() { idp.ForeignKey = false }()
			return logoutForm(idp.LogoutToken(t, "alice", "sid-a1", nil))
		}},
		{"wrong audience", func() url.Values {
			return logoutForm(idp.LogoutToken(t, "alice", "sid-a1", func(c map[string]any) { c["aud"] = "someone-else" }))
		}},
		{"no events claim", func() url.Values {
			return logoutForm(idp.LogoutToken(t, "alice", "sid-a1", func(c map[string]any) { delete(c, "events") }))
		}},
		{"nonce present", func() url.Values {
			return logoutForm(idp.LogoutToken(t, "alice", "sid-a1", func(c map[string]any) { c["nonce"] = "n" }))
		}},
		{"expired", func() url.Values {
			return logoutForm(idp.LogoutToken(t, "alice", "sid-a1", func(c map[string]any) { c["exp"] = 1; c["iat"] = 0 }))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postLogout(srv, tc.form()); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !sessionAlive(srv, alive) {
				t.Fatal("a refused token ended a session")
			}
		})
	}

	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "alice", "sid-a1", nil))); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout with SSO disabled: status = %d, want 503", rec.Code)
	}
}
