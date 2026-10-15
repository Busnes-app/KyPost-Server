package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The operator moves from one provider to another whose numeric subjects
// overlap. The new provider's "1" is somebody else and must not sign in as the
// account the old provider's "1" was linked to, whether that link predates
// stored issuers or not.
func TestSSOIssuerChangeRevokesLinks(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		srv, idp := setupSSOTestServer(t)
		settings := srv.ssoStore.Load()
		settings.IssuerURL = "https://gitea.example"
		if err := srv.ssoStore.Save(settings); err != nil {
			t.Fatal(err)
		}
		victim, err := srv.users.Create(context.Background(), "victim", linkTestPassword, users.RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if legacy {
			err = srv.users.LinkSSO(victim.ID, "1", "victim", "")
		} else {
			err = srv.users.LinkSSOIssuer(settings.IssuerURL, victim.ID, "1", "victim", "")
		}
		if err != nil {
			t.Fatal(err)
		}

		settings.IssuerURL = idp.URL()
		body, _ := json.Marshal(settings)
		rec := httptest.NewRecorder()
		srv.handleAdminSSOPut(rec, httptest.NewRequest(http.MethodPut, "/api/admin/sso", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("legacy=%v: PUT status %d: %s", legacy, rec.Code, rec.Body.String())
		}
		if u, _ := srv.users.Get(victim.ID); !u.SSOLinkRevoked() {
			t.Errorf("legacy=%v: issuer change left the old link live", legacy)
		}

		idp.SetClaims(map[string]any{"sub": "1", "preferred_username": "stranger"})
		if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
			t.Fatalf("legacy=%v: colliding subject from the new issuer: status %d, want 403", legacy, rec.Code)
		}
	}
}

// Startup binds links stored without an issuer to the configured one, the only
// issuer the old lookup could have resolved them for after this upgrade.
func TestPrepareBindsUnboundSSOLinks(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	victim, err := srv.users.Create(context.Background(), "victim", linkTestPassword, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.users.LinkSSO(victim.ID, "1", "victim", ""); err != nil {
		t.Fatal(err)
	}
	srv.bindUnboundSSOLinks()
	if u, _ := srv.users.Get(victim.ID); u.SSOIssuer != idp.URL() {
		t.Fatalf("unbound link bound to %q, want %q", u.SSOIssuer, idp.URL())
	}
	idp.SetClaims(map[string]any{"sub": "1", "preferred_username": "victim"})
	if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusFound {
		t.Fatalf("bound link sign-in: status %d: %s", rec.Code, rec.Body.String())
	}
}
