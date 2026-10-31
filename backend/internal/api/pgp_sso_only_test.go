package api

import (
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
	// The refused attempt above did not consume it, but it minted a new
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
