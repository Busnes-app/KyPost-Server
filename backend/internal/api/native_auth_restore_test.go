//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Construct partial-marker snapshots as well as complete accounts. Production
// provisioning always writes both; either restored marker must close admission.
func markAuthRestoreNative(t *testing.T, s *Server, id, marker string) {
	t.Helper()
	path := filepath.Join(s.configDir, "users.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version int          `json:"version"`
		Users   []users.User `json:"users"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for i := range f.Users {
		if f.Users[i].ID == id {
			if marker != "source" {
				f.Users[i].NativeMailboxIssuer = s.ssoStore.Load().IssuerURL
				if f.Users[i].NativeMailboxIssuer == "" {
					f.Users[i].NativeMailboxIssuer = "https://idp.example"
				}
			}
			if marker != "issuer" {
				f.Users[i].NativeMailboxSource = "test-restored-source"
			}
		}
	}
	raw, err = json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func holdAuthRestore(t *testing.T, s *Server) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), []byte("malformed hold"), 0600); err != nil {
		t.Fatal(err)
	}
	s.nativeMail = false // Disabling the transport cannot disable quarantine.
}

func TestNativeAuthRestoreDeniesSessionsAndPassword(t *testing.T) {
	for _, marker := range []string{"both", "issuer", "source"} {
		t.Run(marker, func(t *testing.T) {
			t.Setenv("STATE_DIR", t.TempDir())
			s, u := newTestServerWithUser(t)
			legacy, err := s.users.Create(context.Background(), "recovery-operator", "recovery-operator-testpassword", users.RoleAdmin)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.users.ClearMustChangePassword(legacy.ID); err != nil {
				t.Fatal(err)
			}
			// A held native admin's old cookie must confer no instance authority.
			if _, err := s.users.SetRole(u.ID, users.RoleAdmin); err != nil {
				t.Fatal(err)
			}
			cookieRec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/auth/login", nil)
			if err := s.startSession(cookieRec, req, u.ID); err != nil {
				t.Fatal(err)
			}
			cookie := sessionCookieFrom(cookieRec)
			markAuthRestoreNative(t, s, u.ID, marker)
			holdAuthRestore(t, s)
			before, err := os.ReadFile(filepath.Join(s.configDir, "users.json"))
			if err != nil {
				t.Fatal(err)
			}
			login := doJSON(s, s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": u.Username, "password": "session-tester-testpassword"})
			if login.Code != http.StatusUnauthorized || sessionCookieFrom(login) != nil {
				t.Fatalf("held login: %d", login.Code)
			}
			if err := s.startSession(httptest.NewRecorder(), req, u.ID); err == nil {
				t.Fatal("shared session sink admitted held native account")
			}
			if w := gatedCall(t, s, cookie, "GET", "/api/users", "", ""); w.Code != http.StatusUnauthorized {
				t.Fatalf("held admin cookie: %d", w.Code)
			}
			after, err := os.ReadFile(filepath.Join(s.configDir, "users.json"))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refused authentication changed account credentials", err)
			}
			// A separate legacy recovery operator remains able to administer the server.
			rec := httptest.NewRecorder()
			if err := s.startSession(rec, req, legacy.ID); err != nil {
				t.Fatal(err)
			}
			if w := gatedCall(t, s, sessionCookieFrom(rec), "GET", "/api/users", "", ""); w.Code != 200 {
				t.Fatalf("legacy recovery operator: %d", w.Code)
			}

		})
	}
}

func TestNativeAuthRestorePreservesOutstandingMFA(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s := newTestServer(t)
	u, err := s.users.Create(context.Background(), "held-mfa", "pw-held-mfa-testpassword", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	secret, codes := enrollTOTP(t, s, u.ID)
	ch, err := s.mfaChallenges.Create(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	markAuthRestoreNative(t, s, u.ID, "both")
	holdAuthRestore(t, s)
	before, err := s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"totp", "recovery"} {
		handler := s.handleMFATOTP
		code := totpCodeForTest(t, secret, time.Now())
		if method == "recovery" {
			handler = s.handleMFARecoveryCode
			code = codes[0]
		}
		w := doJSON(s, handler, "POST", "/api/auth/mfa/"+method, map[string]string{"challengeId": ch.ID, "code": code})
		if w.Code != 401 || sessionCookieFrom(w) != nil {
			t.Fatalf("held %s: %d", method, w.Code)
		}
	}
	after, err := s.users.Get(u.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("held MFA spent recovery material", err)
	}
	if _, ok := s.mfaChallenges.Get(ch.ID); !ok {
		t.Fatal("held MFA consumed outstanding challenge")
	}
}

func TestNativeAuthRestoreDeniesSSOAndPendingStepUp(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s, idp := setupSSOTestServer(t)
	cookie := ssoSignIn(t, s, idp, "held-sso", "sid-old")
	u, err := s.users.GetBySSOSub("held-sso")
	if err != nil {
		t.Fatal(err)
	}
	challenge := challengeFrom(t, gatedCall(t, s, cookie, "POST", "/api/auth/step-up", "{}", ""))
	start := gatedCall(t, s, cookie, "POST", "/api/auth/oidc/step-up", `{"challenge":"`+challenge+`"}`, "")
	if start.Code != 200 {
		t.Fatalf("step-up start: %d", start.Code)
	}
	markAuthRestoreNative(t, s, u.ID, "both")
	holdAuthRestore(t, s)
	idp.SetClaims(freshClaims("held-sso", "sid-new"))
	if w := redeemSSOLink(t, s, idp, cookie, start); w.Code != 401 || sessionCookieFrom(w) != nil {
		t.Fatalf("held step-up callback: %d", w.Code)
	}
	s.stepUpMu.Lock()
	verified := s.stepUps[challenge].verified
	s.stepUpMu.Unlock()
	if verified {
		t.Fatal("held SSO callback authorized step-up")
	}
	if w := runSSOFlow(t, s, idp, nil, false); w.Code != 403 || sessionCookieFrom(w) != nil {
		t.Fatalf("held signed SSO callback: %d", w.Code)
	}
}

func TestNativeAuthRestoreProvisionedDerivedPushAndQR(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "auth-native-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	password := "native-auth-testpassword"
	if _, err := s.users.SetPassword(context.Background(), u.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	salt := s.syntheticLoginSalt(u.Username)
	authSecret, err := users.DeriveAuthSecret(password, salt, clientLoginIterations)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.users.UpgradeToDerivedAuth(context.Background(), u.ID, password, authSecret, salt, clientLoginIterations); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(s, s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": u.Username, "authSecret": authSecret}); w.Code != 200 || sessionCookieFrom(w) == nil {
		t.Fatalf("live derived native login: %d", w.Code)
	}
	if _, err := s.users.SetPushMFAEnabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	ch, err := s.mfaChallenges.Create(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.mfaChallenges.ResolvePushWithMatch(ch.ID, "test-approved-device", true, ch.MatchDigits); err != nil {
		t.Fatal(err)
	}
	identity, err := pgpmail.GenerateIdentity("Restore QR", "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := identity.SealPrivateKey(s.pgpPrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.users.SetPGPIdentity(u.ID, identity.Fingerprint, identity.KeyID, identity.ArmoredPublicKey, sealed, "generated", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	token, _, err := s.createPairingToken(u.ID, pairingPurposePGPQRKey, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.decodeAndVerifyPairingToken(token, pairingPurposePGPQRKey, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	holdAuthRestore(t, s)
	before, err := s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	login := doJSON(s, s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": u.Username, "authSecret": authSecret})
	if login.Code != 401 || sessionCookieFrom(login) != nil {
		t.Fatalf("held derived login: %d", login.Code)
	}
	push := doJSON(s, s.handlePushFinish, "POST", "/api/auth/mfa/push/finish", map[string]string{"challengeId": ch.ID, "finishSecret": ch.FinishSecret})
	if push.Code != 401 || sessionCookieFrom(push) != nil {
		t.Fatalf("held approved push: %d", push.Code)
	}
	qr := httptest.NewRecorder()
	s.routes().ServeHTTP(qr, httptest.NewRequest("GET", "/api/pgp/qr/key?t="+token, nil))
	if qr.Code != 403 {
		t.Fatalf("held historical QR: %d", qr.Code)
	}
	if !s.consumeQRToken(claims.Nonce) {
		t.Fatal("hold refusal spent QR token")
	}
	after, err := s.users.Get(u.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("held grants changed account/key material", err)
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile)); err != nil {
		t.Fatal("hold changed", err)
	}
}
