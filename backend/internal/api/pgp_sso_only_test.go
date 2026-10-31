package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// An auto-provisioned SSO account has no password. Its PGP key writes must be
// confirmed with KySignOn, bound to the exact request, instead of refusing a
// password the account cannot have — which left such accounts unable to mint
// a key at all.
func TestSSOOnlyAccountMintsAPGPIdentityThroughKySignOn(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-1")
	u, err := srv.users.GetBySSOSub("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.HasLocalCredential() {
		t.Fatalf("precondition: an auto-provisioned account stores a credential: %+v", u)
	}

	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"publicKey":        id.ArmoredPublicKey,
		"wrapped":          `{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"c2FsdA==","iv":"aXY=","ciphertext":"Y3Q="}`,
		"source":           "generated",
		"expectedRevision": u.PGPRevision,
	})
	body := string(payload)

	challenge := challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/client", body, ""))
	if rec := proveStepUp(t, srv, idp, cookie, challenge, freshClaims("alice", "sid-2")); rec.Code != http.StatusOK {
		t.Fatalf("proof callback: status %d: %s", rec.Code, rec.Body.String())
	}
	// The grant is for this request; a different body cannot spend it.
	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/client", strings.Replace(body, `"generated"`, `"imported"`, 1), challenge); rec.Code != http.StatusForbidden {
		t.Fatalf("grant spent on a different request: status %d: %s", rec.Code, rec.Body.String())
	}
	// The grant is still unspent, but asking again without it mints a fresh
	// challenge for the session, which replaces the old one.
	challenge = challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/client", body, ""))
	if rec := proveStepUp(t, srv, idp, cookie, challenge, freshClaims("alice", "sid-3")); rec.Code != http.StatusOK {
		t.Fatalf("second proof callback: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/client", body, challenge); rec.Code != http.StatusOK {
		t.Fatalf("identity/client with the grant: status %d: %s", rec.Code, rec.Body.String())
	}
	after, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PGPFingerprint != id.Fingerprint || after.PGPProtection() != "client" {
		t.Fatalf("identity not stored client-protected: fingerprint %q protection %q", after.PGPFingerprint, after.PGPProtection())
	}

	// Changing the passphrase is a rewrap, confirmed the same way.
	rewrap, _ := json.Marshal(map[string]any{
		"wrapped":             `{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"bmV3","iv":"aXY=","ciphertext":"Y3Q="}`,
		"expectedFingerprint": id.Fingerprint, "expectedRevision": after.PGPRevision,
	})
	challenge = challengeFrom(t, gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/rewrap", string(rewrap), ""))
	if rec := proveStepUp(t, srv, idp, cookie, challenge, freshClaims("alice", "sid-4")); rec.Code != http.StatusOK {
		t.Fatalf("rewrap proof callback: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/rewrap", string(rewrap), challenge); rec.Code != http.StatusOK {
		t.Fatalf("rewrap with the grant: status %d: %s", rec.Code, rec.Body.String())
	}
	if after, err = srv.users.Get(u.ID); err != nil {
		t.Fatal(err)
	}

	// A keyring-sized body still fits the PGP digest bound, not the 64 KiB one
	// the backup routes use.
	big, _ := json.Marshal(map[string]any{
		"wrapped": strings.Repeat("x", 100<<10), "expectedFingerprint": id.Fingerprint, "expectedRevision": after.PGPRevision,
	})
	if rec := gatedCall(t, srv, cookie, http.MethodPost, "/api/pgp/identity/rewrap", string(big), ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "sso_step_up_required") {
		t.Fatalf("large rewrap body: status %d, want a KySignOn challenge: %.200s", rec.Code, rec.Body.String())
	}
}

// The client is told the account has no password, so it asks for the
// encryption passphrase instead.
func TestSSOOnlyAccountIsToldItHasNoPassword(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-1")

	rec := gatedCall(t, srv, cookie, http.MethodGet, "/api/auth/me", "", "")
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me["hasPassword"] != false {
		t.Fatalf("/api/auth/me for an SSO-only session: %s", rec.Body.String())
	}
}

// A generic provider cannot confirm an action, and an SSO-only account there
// has no password either: it gets a plain answer, not "invalid credentials".
func TestSSOOnlyGenericProviderAccountIsToldKySignOnIsNeeded(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := signInWith(t, srv, idp, map[string]any{"sub": "kc", "preferred_username": "kc"})

	rec := gatedCall(t, srv, cookie, http.MethodDelete, "/api/pgp/identity", `{}`, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "kysignon_required") {
		t.Fatalf("generic-provider SSO-only step-up: status %d: %s", rec.Code, rec.Body.String())
	}
}

// pgpStepUpRoutes is every route that reaches requirePGPStepUp, with the
// smallest body that gets each handler as far as its step-up.
var pgpStepUpRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/api/pgp/identity/client", `{"publicKey":"x","wrapped":"x"}`},
	{http.MethodPost, "/api/pgp/identity/rewrap", `{"wrapped":"x"}`},
	{http.MethodPut, "/api/pgp/identity/envelope/recovery", `{"envelope":"x"}`},
	{http.MethodDelete, "/api/pgp/identity/envelope/recovery", `{}`},
	{http.MethodDelete, "/api/pgp/identity", `{}`},
	{http.MethodPut, "/api/pgp/incoming", `{"enabled":false}`},
}

// Every gated route is wrapped in an action digest, so an SSO-only KySignOn
// session gets a challenge there rather than the 500 an unwrapped route gives.
func TestSSOOnlyAccountIsChallengedOnEveryPGPStepUpRoute(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-1")
	for _, route := range pgpStepUpRoutes {
		rec := gatedCall(t, srv, cookie, route.method, route.path, route.body, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "sso_step_up_required") {
			t.Errorf("%s %s: status %d, want a KySignOn challenge: %s", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
}

// The KySignOn arm is for accounts with no password only. An account that has
// one keeps proving it, even in a KySignOn session: the password is also what
// its key is wrapped under, and a session alone must not replace that.
func TestPasswordAccountInAKySignOnSessionStillNeedsThePassword(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, srv, idp, "bob", "sid-1")
	bob, err := srv.users.GetBySSOSub("bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.SetPassword(context.Background(), bob.ID, "bob-local-password-123", false, nil); err != nil {
		t.Fatal(err)
	}
	clearMustChangePassword(t, srv, bob.ID)
	for _, route := range pgpStepUpRoutes {
		rec := gatedCall(t, srv, cookie, route.method, route.path, route.body, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "invalid credentials") {
			t.Errorf("%s %s without the password: status %d, want the credential refusal: %s", route.method, route.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "sso_step_up_required") {
			t.Errorf("%s %s offered KySignOn to an account with a password", route.method, route.path)
		}
	}
}
