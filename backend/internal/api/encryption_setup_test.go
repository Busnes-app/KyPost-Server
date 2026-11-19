package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	setupWrapped  = `{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"c2FsdA==","iv":"aXY=","ciphertext":"V1JBUFBFRC1QQVNTV09SRA=="}`
	setupRecovery = `{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"cmVj","iv":"aXY=","ciphertext":"UkVDT1ZFUlktU0xPVA=="}`
)

func savePolicy(t *testing.T, srv *Server, p EncryptionPolicy) {
	t.Helper()
	raw, _ := json.Marshal(p)
	if err := srv.globalStore.SetSetting(encryptionPolicySetting, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// withAuditLog gives the server the audit log NewServer opens in production.
func withAuditLog(t *testing.T, srv *Server) {
	t.Helper()
	var err error
	if srv.backup, err = backup.New(backup.Dirs{Config: srv.configDir, State: srv.stateDir, Secret: srv.configDir}, config.BackupConfig{Keep: 1}, srv.globalStore, "test"); err != nil {
		t.Fatal(err)
	}
}

// signInSession mints a session the way every sign-in does.
func signInSession(t *testing.T, srv *Server, sess Session) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := srv.mintSession(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), sess); err != nil {
		t.Fatal(err)
	}
	return sessionCookieFrom(rec)
}

func setupUser(t *testing.T, srv *Server, name string) users.User {
	t.Helper()
	u, err := srv.users.Create(context.Background(), name, name+"-password-1234", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	clearMustChangePassword(t, srv, u.ID)
	return u
}

func completeKeySetup(t *testing.T, srv *Server, userID string) {
	t.Helper()
	if _, err := srv.users.SetPGPIdentityClientProtected(userID, "FPR"+userID, "KID", "-----BEGIN PGP PUBLIC KEY BLOCK-----\npub\n-----END PGP PUBLIC KEY BLOCK-----", setupWrapped, "generated", "2026-11-18T00:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.SetPGPWrappedEnvelope(userID, users.EnvelopeSlotRecovery, setupRecovery, "2026-11-18T00:00:00Z", "", nil); err != nil {
		t.Fatal(err)
	}
}

func deviceGet(srv *Server, id, secret, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	setDeviceHeaders(req, id, secret)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func heldForSetup(rec *httptest.ResponseRecorder) bool {
	var body struct{ Error string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code == http.StatusForbidden && body.Error == "encryption_setup_required"
}

func meSetup(t *testing.T, srv *Server, c *http.Cookie) encryptionSetup {
	t.Helper()
	var me struct{ EncryptionSetup encryptionSetup }
	rec := gatedCall(t, srv, c, http.MethodGet, "/api/auth/me", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(rec.Body.String())
	}
	return me.EncryptionSetup
}

// adminPut calls an admin handler as admin, past the step-up its route adds.
func adminPut(srv *Server, admin users.User, handler http.HandlerFunc, path, id string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := authedRequestForTest(httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw)), admin)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func auditOutcomes(t *testing.T, srv *Server, action string) []string {
	t.Helper()
	rows, err := srv.globalStore.RecentBackupAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Action == action {
			out = append(out, r.Outcome)
		}
	}
	return out
}

// The owner's default: an encryption key is Required and incoming encryption
// starts on. An install that never saved a policy holds a keyless account's
// mail at sign-in.
func TestShippedEncryptionPolicyRequiresAKey(t *testing.T) {
	if shippedEncryptionPolicy != (EncryptionPolicy{Key: policyRequired, Incoming: policyOn}) {
		t.Fatalf("shipped policy = %+v, want required key, incoming on by default", shippedEncryptionPolicy)
	}
	lowered := defaultEncryptionPolicy
	defaultEncryptionPolicy = shippedEncryptionPolicy
	t.Cleanup(func() { defaultEncryptionPolicy = lowered })
	srv := newTestServer(t)
	u := setupUser(t, srv, "fresh")
	c := signInSession(t, srv, Session{UserID: u.ID})
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); !heldForSetup(rec) {
		t.Fatalf("keyless account read mail under the shipped policy: %d %s", rec.Code, rec.Body)
	}
	if st := meSetup(t, srv, c); !st.Required || st.KeyPolicy != policyRequired || st.IncomingPolicy != policyOn {
		t.Fatalf("/api/auth/me setup = %+v", st)
	}
}

// Upgrading turns Required on for accounts that had no key. A session signed
// in before the change keeps working, so nothing in progress is lost; the next
// sign-in is held, and a paired device is refused with the documented error
// at once rather than syncing mail around the requirement. Finishing setup
// releases both, and recovery is part of finishing.
func TestEncryptionSetupUpgradePath(t *testing.T) {
	srv := newTestServer(t)
	u := setupUser(t, srv, "existing")
	before := signInSession(t, srv, Session{UserID: u.ID})
	devID, secret := pairNativeDevice(t, srv, u.ID, "phone")

	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyOn})

	if rec := gatedCall(t, srv, before, http.MethodGet, "/api/inbox", "", ""); heldForSetup(rec) {
		t.Fatal("a session signed in before the policy change was cut off mid-work")
	}
	after := signInSession(t, srv, Session{UserID: u.ID})
	for _, path := range []string{"/api/inbox", "/api/mail/body?messageId=1", "/api/mail/search?q=x"} {
		if rec := gatedCall(t, srv, after, http.MethodGet, path, "", ""); !heldForSetup(rec) {
			t.Fatalf("next sign-in read %s before setup: %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := deviceGet(srv, devID, secret, "/api/inbox")
	if !heldForSetup(rec) || !strings.Contains(rec.Body.String(), "on the web") {
		t.Fatalf("device was not told to finish setup on the web: %d %s", rec.Code, rec.Body)
	}
	// Setup itself stays reachable.
	if rec := gatedCall(t, srv, after, http.MethodGet, "/api/pgp/bootstrap", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap during setup: %d %s", rec.Code, rec.Body)
	}
	if st := meSetup(t, srv, after); !st.Required || st.HasKey {
		t.Fatalf("setup status before the key = %+v", st)
	}

	if _, err := srv.users.SetPGPIdentityClientProtected(u.ID, "FPR1", "KID", "pub", setupWrapped, "generated", "2026-11-18T00:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if rec := gatedCall(t, srv, after, http.MethodGet, "/api/inbox", "", ""); !heldForSetup(rec) {
		t.Fatal("setup finished without a recovery copy")
	}
	if _, err := srv.users.SetPGPWrappedEnvelope(u.ID, users.EnvelopeSlotRecovery, setupRecovery, "2026-11-18T00:00:00Z", "", nil); err != nil {
		t.Fatal(err)
	}
	if rec := gatedCall(t, srv, after, http.MethodGet, "/api/inbox", "", ""); heldForSetup(rec) {
		t.Fatal("finished setup still held the session")
	}
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); heldForSetup(rec) {
		t.Fatal("finished setup still held the device")
	}
	if st := meSetup(t, srv, after); st.Required || !st.HasKey || !st.HasRecovery {
		t.Fatalf("setup status after = %+v", st)
	}
}

// Optional offers the step with "Not now" and holds nothing; an
// administrator's "Require key setup" holds that one account anyway.
func TestOptionalPolicyOffersAndPerUserRequirementHolds(t *testing.T) {
	srv := newTestServer(t)
	savePolicy(t, srv, EncryptionPolicy{Key: policyOptional, Incoming: policyOff})
	u := setupUser(t, srv, "optional")
	c := signInSession(t, srv, Session{UserID: u.ID})
	if st := meSetup(t, srv, c); st.Required || !st.Offered {
		t.Fatalf("optional policy setup = %+v, want offered only", st)
	}
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); heldForSetup(rec) {
		t.Fatal("optional policy held mail")
	}
	if _, err := srv.users.SetKeySetupRequired(u.ID, true); err != nil {
		t.Fatal(err)
	}
	c = signInSession(t, srv, Session{UserID: u.ID})
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); !heldForSetup(rec) {
		t.Fatal("Require key setup did not hold the next sign-in")
	}
}

// Required incoming encryption holds an account with a key until it is on,
// and refuses turning it off.
func TestRequiredIncomingEncryption(t *testing.T) {
	srv := newTestServer(t)
	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyRequired})
	u := setupUser(t, srv, "incoming")
	completeKeySetup(t, srv, u.ID)
	c := signInSession(t, srv, Session{UserID: u.ID})
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); !heldForSetup(rec) {
		t.Fatal("incoming encryption off, yet setup counted as complete under Required")
	}
	rec := gatedCall(t, srv, c, http.MethodPut, "/api/pgp/incoming", `{"enabled":false}`, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "incoming_encryption_required") {
		t.Fatalf("disabling Required incoming encryption: %d %s", rec.Code, rec.Body)
	}
	if err := config.UpdateUserSettings(srv.userSettingsPath(u.ID), func(s *config.UserSettings) error { s.EncryptIncoming = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); heldForSetup(rec) {
		t.Fatal("incoming encryption on, still held")
	}
}

// Required needs KySignOn: with a generic provider configured it is refused,
// whichever setting asks for it, and saved once discovery shows KySignOn.
// Each change is audited.
func TestEncryptionPolicyRequiredNeedsKySignOn(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	withAuditLog(t, srv)
	admin := setupUser(t, srv, "policy-admin")
	for _, body := range []EncryptionPolicy{{Key: policyRequired, Incoming: policyOn}, {Key: policyOptional, Incoming: policyRequired}} {
		rec := adminPut(srv, admin, srv.handleEncryptionPolicyPut, "/api/admin/encryption-policy", "", body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Needs KySignOn: users signing in through 127.0.0.1") {
			t.Fatalf("Required beside a generic provider %+v: %d %s", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "could not confirm") {
			t.Fatalf("discovery did not run: %s", rec.Body)
		}
	}
	if p := srv.encryptionPolicy(); p.Key != policyOptional {
		t.Fatalf("refused policy was saved: %+v", p)
	}
	if len(auditOutcomes(t, srv, "admin.encryption_policy")) != 0 {
		t.Fatal("a refused change was audited as started")
	}

	idp.KySignOn = true
	sso.SetTransport(idp.Transport()) // drop the cached generic discovery
	rec := adminPut(srv, admin, srv.handleEncryptionPolicyPut, "/api/admin/encryption-policy", "", EncryptionPolicy{Key: policyOptional, Incoming: policyRequired})
	if rec.Code != http.StatusOK {
		t.Fatalf("Required with KySignOn: %d %s", rec.Code, rec.Body)
	}
	if p := srv.encryptionPolicy(); p != (EncryptionPolicy{Key: policyRequired, Incoming: policyRequired}) {
		t.Fatalf("Required incoming did not require the key: %+v", p)
	}
	if !srv.ssoStore.Load().KySignOn {
		t.Fatal("confirmed KySignOn provider not recorded")
	}
	if got := auditOutcomes(t, srv, "admin.encryption_policy"); strings.Join(got, ",") != "success,started" {
		t.Fatalf("audit outcomes = %v", got)
	}
}

// Switching SSO to a generic provider is refused while either policy is
// Required, with the setting to lower named. The server decides KySignOn;
// a request claiming it is ignored.
func TestSSOSwitchToGenericProviderRefusedWhileRequired(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	idp.KySignOn = true
	generic := ssotest.New(t, "generic")
	settings := srv.ssoStore.Load()

	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyRequired})
	put := func(cfg sso.SSOSettings) *httptest.ResponseRecorder {
		body, _ := json.Marshal(cfg)
		rec := httptest.NewRecorder()
		srv.handleAdminSSOPut(rec, httptest.NewRequest(http.MethodPut, "/api/admin/sso", bytes.NewReader(body)))
		return rec
	}
	if rec := put(settings); rec.Code != http.StatusOK || !srv.ssoStore.Load().KySignOn {
		t.Fatalf("saving KySignOn under Required: %d %s", rec.Code, rec.Body)
	}

	switched := settings
	switched.IssuerURL, switched.ClientID, switched.KySignOn = generic.URL(), generic.ClientID, true
	rec := put(switched)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Lower Encryption key and Encrypt incoming mail from Required first") {
		t.Fatalf("switch to a generic provider under Required: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "could not confirm") {
		t.Fatalf("discovery did not run: %s", rec.Body)
	}
	if got := srv.ssoStore.Load(); got.IssuerURL != settings.IssuerURL || !got.KySignOn {
		t.Fatalf("refused switch changed the settings: %+v", got)
	}

	savePolicy(t, srv, EncryptionPolicy{Key: policyOptional, Incoming: policyOn})
	if rec := put(switched); rec.Code != http.StatusOK {
		t.Fatalf("switch once lowered: %d %s", rec.Code, rec.Body)
	}
	if got := srv.ssoStore.Load(); got.IssuerURL != generic.URL() || got.KySignOn {
		t.Fatalf("request's kySignOn claim was stored: %+v", got)
	}
}

// An SSO-only account of a generic provider could never create a key, so it is
// neither held nor offered setup, on the web or a device. A KySignOn SSO-only
// account is held like anyone else.
func TestSetupSkipsSSOOnlyAccountsOfAGenericProvider(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyOn})

	c := signInWith(t, srv, idp, map[string]any{"sub": "kc", "preferred_username": "kc"})
	if st := meSetup(t, srv, c); st.Required || st.Offered {
		t.Fatalf("generic SSO-only setup = %+v", st)
	}
	if rec := gatedCall(t, srv, c, http.MethodGet, "/api/inbox", "", ""); heldForSetup(rec) {
		t.Fatal("generic SSO-only account held for a setup it cannot do")
	}
	kc, err := srv.users.GetBySSOSub("kc")
	if err != nil {
		t.Fatal(err)
	}
	devID, secret := pairNativeDevice(t, srv, kc.ID, "kc-phone")
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); heldForSetup(rec) {
		t.Fatal("generic SSO-only device held")
	}

	k := ssoSignIn(t, srv, idp, "alice", "sid-1")
	if rec := gatedCall(t, srv, k, http.MethodGet, "/api/inbox", "", ""); !heldForSetup(rec) {
		t.Fatal("KySignOn SSO-only account read mail before setup")
	}
}

// Require key setup sets a flag; no administrator route returns key material.
func TestAdminRequireKeySetupNeverExposesKeyMaterial(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	withAuditLog(t, srv)
	completeKeySetup(t, srv, member.ID)

	rec := adminPut(srv, admin, srv.handleUserKeySetup, "/api/users/"+member.ID+"/key-setup", member.ID, map[string]any{"required": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"keySetupRequired":true`) {
		t.Fatalf("require key setup: %d %s", rec.Code, rec.Body)
	}
	if u, _ := srv.users.Get(member.ID); !u.KeySetupRequired {
		t.Fatal("flag not stored")
	}
	if got := auditOutcomes(t, srv, "admin.require_key_setup"); strings.Join(got, ",") != "success,started" {
		t.Fatalf("audit outcomes = %v", got)
	}

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		authRequestAs(srv, req, admin.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Body.String()
	}
	listed := get("/api/users")
	for name, body := range map[string]string{"key-setup": rec.Body.String(), "users": listed, "policy": get("/api/admin/encryption-policy")} {
		for _, secret := range []string{"V1JBUFBFRC1QQVNTV09SRA", "UkVDT1ZFUlktU0xPVA", "pgpPrivateKey", "pgpWrappedEnvelopes", "envelope"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s response exposes %q: %s", name, secret, body)
			}
		}
	}
	if !strings.Contains(listed, `"keySetupRequired":true`) {
		t.Fatalf("users list hides the requirement: %s", listed)
	}
}

// Every save of an enabled provider records whether it is KySignOn, whatever
// the policy, so a later per-user requirement or Required policy holds an
// SSO-only account's paired device as it holds the browser.
func TestKySignOnRecordedOnEverySSOSave(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	idp.KySignOn = true
	savePolicy(t, srv, EncryptionPolicy{Key: policyOptional, Incoming: policyOff})
	body, _ := json.Marshal(srv.ssoStore.Load())
	rec := httptest.NewRecorder()
	srv.handleAdminSSOPut(rec, httptest.NewRequest(http.MethodPut, "/api/admin/sso", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || !srv.ssoStore.Load().KySignOn {
		t.Fatalf("KySignOn provider saved under Optional: %d %s, kySignOn=%v", rec.Code, rec.Body, srv.ssoStore.Load().KySignOn)
	}

	ssoSignIn(t, srv, idp, "alice", "sid-1")
	alice, err := srv.users.GetBySSOSub("alice")
	if err != nil {
		t.Fatal(err)
	}
	devID, secret := pairNativeDevice(t, srv, alice.ID, "alice-phone")
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); heldForSetup(rec) {
		t.Fatal("Optional policy held the device")
	}
	if _, err := srv.users.SetKeySetupRequired(alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); !heldForSetup(rec) {
		t.Fatalf("Require key setup did not hold the SSO-only account's device: %d %s", rec.Code, rec.Body)
	}
	if _, err := srv.users.SetKeySetupRequired(alice.ID, false); err != nil {
		t.Fatal(err)
	}
	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyOn})
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); !heldForSetup(rec) {
		t.Fatalf("Required after an Optional-time SSO save did not hold the device: %d %s", rec.Code, rec.Body)
	}
}

// Settings not marked KySignOn (saved before detection, or discovery failed)
// are checked again by a device request, which then holds like the browser.
func TestDeviceRechecksAProviderNotMarkedKySignOn(t *testing.T) {
	srv, idp := setupSSOTestServer(t) // saved without the mark
	idp.KySignOn = true
	savePolicy(t, srv, EncryptionPolicy{Key: policyRequired, Incoming: policyOn})
	ssoSignIn(t, srv, idp, "alice", "sid-1")
	alice, err := srv.users.GetBySSOSub("alice")
	if err != nil {
		t.Fatal(err)
	}
	devID, secret := pairNativeDevice(t, srv, alice.ID, "alice-phone")
	if rec := deviceGet(srv, devID, secret, "/api/inbox"); !heldForSetup(rec) {
		t.Fatalf("unmarked KySignOn provider: device not held: %d %s", rec.Code, rec.Body)
	}
	if !srv.ssoStore.Load().KySignOn {
		t.Fatal("recheck did not record the KySignOn provider")
	}
}
