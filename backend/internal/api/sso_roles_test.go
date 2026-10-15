package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// signInWith completes a provider login carrying exactly these claims.
func signInWith(t *testing.T, srv *Server, idp *ssotest.IdP, claims map[string]any) *http.Cookie {
	t.Helper()
	idp.SetClaims(claims)
	rec := runSSOFlow(t, srv, idp, nil, false)
	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in: status = %d (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatal("sign-in minted no session")
	}
	return c
}

// effectiveRole is what the session may do, read the way every admin route
// and /api/auth/me read it.
func effectiveRole(t *testing.T, srv *Server, c *http.Cookie) users.Role {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(c)
	ac, ok := srv.currentUser(req)
	if !ok {
		t.Fatal("session is not alive")
	}
	rec := httptest.NewRecorder()
	srv.handleMe(rec, req)
	var me map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me["role"] != string(ac.Role) {
		t.Fatalf("/api/auth/me reports role %v, the request context %q", me["role"], ac.Role)
	}
	return ac.Role
}

func TestSSOAppRoleIsTheOnlyAdminSignal(t *testing.T) {
	srv, idp := setupSSOTestServer(t)

	admin := signInWith(t, srv, idp, map[string]any{"sub": "a", "preferred_username": "a", "roles": []string{"kypost.admin"}})
	if u, _ := srv.users.GetBySSOSub("a"); u.Role != users.RoleAdmin {
		t.Fatalf("provisioned role = %q, want admin", u.Role)
	}
	if got := effectiveRole(t, srv, admin); got != users.RoleAdmin {
		t.Fatalf("session role = %q, want admin", got)
	}

	// With roles present, the legacy global-admin claim and every generic
	// admin group are ignored.
	legacy := signInWith(t, srv, idp, map[string]any{
		"sub": "b", "preferred_username": "b",
		"roles":        []string{"notes.admin"},
		"role":         "admin",
		"admin":        true,
		"groups":       []string{"admins"},
		"ak_groups":    []string{"authentik default admins"},
		"realm_access": map[string]any{"roles": []string{"admin"}},
	})
	if u, _ := srv.users.GetBySSOSub("b"); u.Role != users.RoleUser {
		t.Fatalf("legacy admin claims provisioned role %q, want user", u.Role)
	}
	if got := effectiveRole(t, srv, legacy); got != users.RoleUser {
		t.Fatalf("legacy admin claims gave session role %q, want user", got)
	}
}

func TestSSOSessionAdminIsBoundedByTokenAndAccount(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey

	// The directory made this account an admin. A token without the app
	// role still signs in, but the session is a user's.
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser("c", "c", true, "kypost.admin")))
	capped := signInWith(t, srv, idp, map[string]any{"sub": "c", "preferred_username": "c", "roles": []string{}})
	if got := effectiveRole(t, srv, capped); got != users.RoleUser {
		t.Fatalf("session without the app role = %q, want user", got)
	}
	if u, _ := srv.users.GetBySSOSub("c"); u.Role != users.RoleAdmin {
		t.Fatal("a login changed the account's role")
	}
	full := signInWith(t, srv, idp, map[string]any{"sub": "c", "preferred_username": "c", "roles": []string{"kypost.admin"}})
	if got := effectiveRole(t, srv, full); got != users.RoleAdmin {
		t.Fatalf("session with the app role = %q, want admin", got)
	}

	// The other bound: the token says admin but the account is a user. A
	// login never promotes an existing account, and the session is a user's.
	signInWith(t, srv, idp, map[string]any{"sub": "d", "preferred_username": "d", "roles": []string{}})
	promoted := signInWith(t, srv, idp, map[string]any{"sub": "d", "preferred_username": "d", "roles": []string{"kypost.admin"}})
	if u, _ := srv.users.GetBySSOSub("d"); u.Role != users.RoleUser {
		t.Fatal("a login promoted an existing account")
	}
	if got := effectiveRole(t, srv, promoted); got != users.RoleUser {
		t.Fatalf("session for a user account with an admin token = %q, want user", got)
	}
}

func setAdminGroup(t *testing.T, srv *Server, group string) {
	t.Helper()
	settings := srv.ssoStore.Load()
	settings.AdminGroup = group
	if err := srv.ssoStore.Save(settings); err != nil {
		t.Fatal(err)
	}
}

// A generic provider's shared "admins" group belongs to whatever else uses that
// provider; only a group the operator names makes a KyPost administrator.
func TestSSOGenericAdminGroupIsOperatorConfigured(t *testing.T) {
	srv, idp := setupSSOTestServer(t)

	for _, claims := range []map[string]any{
		{"sub": "e1", "preferred_username": "e1", "groups": []string{"admins"}},
		{"sub": "e2", "preferred_username": "e2", "ak_groups": []string{"authentik Admins"}},
		{"sub": "e3", "preferred_username": "e3", "realm_access": map[string]any{"roles": []string{"administrator"}}},
	} {
		c := signInWith(t, srv, idp, claims)
		if u, _ := srv.users.GetBySSOSub(claims["sub"].(string)); u.Role != users.RoleUser {
			t.Fatalf("%v provisioned role %q, want user", claims, u.Role)
		}
		if got := effectiveRole(t, srv, c); got != users.RoleUser {
			t.Fatalf("%v gave session role %q, want user", claims, got)
		}
	}

	// An existing administrator keeps admin with no group configured.
	if _, err := srv.users.CreateSSOUserIssuer(idp.URL(), "g", users.RoleAdmin, "g", "g", ""); err != nil {
		t.Fatal(err)
	}
	existing := signInWith(t, srv, idp, map[string]any{"sub": "g", "preferred_username": "g", "groups": []string{"admins"}})
	if got := effectiveRole(t, srv, existing); got != users.RoleAdmin {
		t.Fatalf("existing admin's session role = %q, want admin", got)
	}

	setAdminGroup(t, srv, "/kypost-admins")
	granted := signInWith(t, srv, idp, map[string]any{"sub": "h", "preferred_username": "h", "groups": []string{"/kypost-admins"}})
	if u, _ := srv.users.GetBySSOSub("h"); u.Role != users.RoleAdmin {
		t.Fatalf("configured group provisioned role %q, want admin", u.Role)
	}
	if got := effectiveRole(t, srv, granted); got != users.RoleAdmin {
		t.Fatalf("configured group gave session role %q, want admin", got)
	}
	shared := signInWith(t, srv, idp, map[string]any{"sub": "i", "preferred_username": "i", "groups": []string{"admins", "/KyPost-Admins"}})
	if got := effectiveRole(t, srv, shared); got != users.RoleUser {
		t.Fatalf("unconfigured group gave session role %q, want user", got)
	}
	// Once a group is named, it is the only generic admin signal, ceiling included.
	if got := effectiveRole(t, srv, signInWith(t, srv, idp, map[string]any{"sub": "g", "preferred_username": "g", "groups": []string{"admins"}})); got != users.RoleUser {
		t.Fatalf("admin outside the configured group kept session role %q", got)
	}
	// A bare legacy role claim, with no roles array and no groups, is not
	// a generic admin signal either.
	plain := signInWith(t, srv, idp, map[string]any{"sub": "f", "preferred_username": "f", "role": "admin"})
	if got := effectiveRole(t, srv, plain); got != users.RoleUser {
		t.Fatalf("legacy role claim alone gave role %q, want user", got)
	}
}
