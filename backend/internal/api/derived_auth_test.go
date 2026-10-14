package api

import (
	"fmt"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func loginParams(t *testing.T, srv *Server, username string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login-params?username="+username, nil)
	rec := httptest.NewRecorder()
	srv.handleLoginParams(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login-params: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Salt       string `json:"salt"`
		Iterations int    `json:"iterations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode login-params: %v", err)
	}
	return out.Salt, out.Iterations
}

var testAuthSecrets sync.Map

// testAuthSecret is what a browser sends for username's password: the
// derivation under the login parameters the server hands out. Cached, since
// every derivation is a full PBKDF2 stretch.
func testAuthSecret(srv *Server, username, password string) string {
	salt, iterations := srv.loginParamsFor(username)
	key := password + "\x00" + salt + "\x00" + strconv.Itoa(iterations)
	if v, ok := testAuthSecrets.Load(key); ok {
		return v.(string)
	}
	v, err := users.DeriveAuthSecret(password, salt, iterations)
	if err != nil {
		panic(err)
	}
	testAuthSecrets.Store(key, v)
	return v
}

// authSecretFor is what a browser sends when the account whose password this is
// confirms an action: the derivation under that account's salt. A password no
// account holds yields a well-formed wrong secret. Two accounts sharing the
// password are reported as AMBIGUOUS; use userAuthSecret/testAuthSecret instead.
func authSecretFor(srv *Server, password string) string {
	all, err := srv.users.List()
	if err != nil {
		panic(err)
	}
	match := ""
	for _, u := range all {
		if !u.UsesDerivedAuth() {
			continue
		}
		secret := testAuthSecret(srv, u.Username, password)
		if ok, _ := users.VerifyAuthSecret(context.Background(), u, secret); ok {
			if match != "" {
				// Not a panic: that would abort every later test in the binary.
				fmt.Fprintln(os.Stderr, "AMBIGUOUS authSecretFor: password "+password+" is shared by several accounts")
			}
			match = secret
		}
	}
	if match == "" {
		return testAuthSecret(srv, "", password)
	}
	return match
}

// userAuthSecret is testAuthSecret for an account known by id.
func userAuthSecret(srv *Server, userID, password string) string {
	u, err := srv.users.Get(userID)
	if err != nil {
		panic(err)
	}
	return testAuthSecret(srv, u.Username, password)
}

// sessionAuthSecret is userAuthSecret for the account a session token belongs to.
func sessionAuthSecret(srv *Server, token, password string) string {
	srv.sessMu.Lock()
	id := srv.sessions[token].UserID
	srv.sessMu.Unlock()
	return userAuthSecret(srv, id, password)
}

// verifyTestPassword reports whether password signs u in.
func verifyTestPassword(srv *Server, u users.User, password string) (bool, error) {
	return users.VerifyAuthSecret(context.Background(), u, testAuthSecret(srv, u.Username, password))
}

// passwordChangeBody is the /api/auth/password body the SPA sends.
func passwordChangeBody(srv *Server, username, oldPassword, newPassword string) map[string]any {
	const salt = "bmV3LXRlc3Qtc2FsdC0xNg==" // "new-test-salt-16"
	iterations := users.LoginIterations()
	newSecret, err := users.DeriveAuthSecret(newPassword, salt, iterations)
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"oldAuthSecret": testAuthSecret(srv, username, oldPassword),
		"newAuthSecret": newSecret,
		"newLoginSalt":  salt,
		"newIterations": iterations,
	}
}

func postLogin(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(raw))
	req.RemoteAddr = "203.0.113.11:5000"
	rec := httptest.NewRecorder()
	srv.handleLogin(rec, req)
	return rec
}

// TestLoginParamsDoesNotRevealAccountExistence is the property that makes the
// pre-login handshake safe to expose.
//
// The endpoint is unauthenticated and takes a username, so if its answer differed
// for a real account it would be a free account-enumeration oracle — worse than
// the timing oracle equalizeLoginTiming exists to close, because it needs no
// measurement at all.
func TestLoginParamsDoesNotRevealAccountExistence(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.users.Create(context.Background(), "realuser", "correct-horse-battery-staple", users.RoleUser); err != nil {
		t.Fatalf("Create: %v", err)
	}

	realSalt, realIter := loginParams(t, srv, "realuser")
	fakeSalt, fakeIter := loginParams(t, srv, "nosuchuser")

	if realSalt == "" || fakeSalt == "" {
		t.Fatal("login-params returned an empty salt; the client cannot derive anything")
	}
	if len(realSalt) != len(fakeSalt) {
		t.Errorf("salt length differs by account existence: real %d vs unknown %d", len(realSalt), len(fakeSalt))
	}
	if realIter != fakeIter {
		t.Errorf("iteration count differs by account existence: real %d vs unknown %d", realIter, fakeIter)
	}
	if realSalt == fakeSalt {
		t.Error("two different usernames got the same salt; salts must be per-account")
	}

	// Stability matters as much as shape: a salt that changed per request would
	// be a louder oracle than returning nothing.
	againSalt, _ := loginParams(t, srv, "nosuchuser")
	if againSalt != fakeSalt {
		t.Error("the synthetic salt for an unknown username is not stable across requests")
	}

	// And the response must carry no derivation/legacy hint. Once every account
	// has converted, such a field would mean "no such user".
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login-params?username=realuser", nil)
	rec := httptest.NewRecorder()
	srv.handleLoginParams(rec, req)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, forbidden := range []string{"derivation", "legacy", "exists", "authDerivation"} {
		if _, present := raw[forbidden]; present {
			t.Errorf("login-params response carries %q, which is an account-existence oracle", forbidden)
		}
	}
}

// TestLoginAcceptsOnlyTheAuthSecret: the derived secret signs in; the correct
// plaintext password, alone or alongside a wrong secret, is refused exactly
// like a wrong credential.
func TestLoginAcceptsOnlyTheAuthSecret(t *testing.T) {
	srv := newTestServer(t)
	const pw = "correct-horse-battery-staple"
	if _, err := srv.users.Create(context.Background(), "derived-user", pw, users.RoleUser); err != nil {
		t.Fatalf("Create: %v", err)
	}
	salt, iterations := loginParams(t, srv, "derived-user")
	authSecret := deriveAuthSecretForTest(t, pw, salt, iterations)

	if rec := postLogin(t, srv, map[string]any{"username": "derived-user", "authSecret": authSecret}); rec.Code != http.StatusOK {
		t.Fatalf("auth secret login: status %d (%s)", rec.Code, rec.Body.String())
	}
	wrong := postLogin(t, srv, map[string]any{"username": "derived-user", "authSecret": strings0(64)})
	for name, body := range map[string]map[string]any{
		"password only":                {"username": "derived-user", "password": pw},
		"password with a wrong secret": {"username": "derived-user", "password": pw, "authSecret": strings0(64)},
	} {
		rec := postLogin(t, srv, body)
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != wrong.Body.String() {
			t.Errorf("%s: %d %q, want the wrong-credential 401 %q", name, rec.Code, rec.Body.String(), wrong.Body.String())
		}
	}
}

// TestLegacyAccountIsRefusedLikeAnUnknownUser: an account still holding a
// plaintext-password hash cannot sign in with any credential, and the public
// endpoints answer exactly as for a nonexistent user or a wrong password. The
// refusal is logged by user_id only.
func TestLegacyAccountIsRefusedLikeAnUnknownUser(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServer(t)
	logger, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger
	const pw = "correct-horse-battery-staple"
	u, err := srv.users.Create(context.Background(), "legacy-user", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plaintextHash, err := users.HashPassword(context.Background(), pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	plantLegacyCredential(t, srv, u.ID, plaintextHash)

	if salt, _ := loginParams(t, srv, "legacy-user"); salt != srv.syntheticLoginSalt("legacy-user") {
		t.Error("login-params served a legacy account something other than the synthetic salt")
	}
	unknown := postLogin(t, srv, map[string]any{"username": "no-such-user", "authSecret": strings0(64)})
	for name, body := range map[string]map[string]any{
		"plaintext password":                {"username": "legacy-user", "password": pw},
		"derived secret":                    {"username": "legacy-user", "authSecret": testAuthSecret(srv, "legacy-user", pw)},
		"the stored hash input as a secret": {"username": "legacy-user", "authSecret": pw},
	} {
		rec := postLogin(t, srv, body)
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() || rec.Header().Get("Content-Type") != unknown.Header().Get("Content-Type") {
			t.Errorf("%s: %d %q, want the unknown-user answer %d %q", name, rec.Code, rec.Body.String(), unknown.Code, unknown.Body.String())
		}
	}
	if !strings.Contains(logs.String(), u.ID) || !strings.Contains(logs.String(), "password reset required") {
		t.Errorf("legacy refusal was not logged by user_id: %q", logs.String())
	}
	if strings.Contains(logs.String(), pw) || strings.Contains(logs.String(), plaintextHash) {
		t.Error("the log carries the password or its hash")
	}
	list := httptest.NewRecorder()
	srv.handleUsersList(list, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	if !strings.Contains(list.Body.String(), `"needsPasswordReset":true`) {
		t.Errorf("admin users list does not flag the legacy account: %s", list.Body.String())
	}
}

// plantLegacyCredential rewrites an account into the pre-derivation shape: a
// hash of the plaintext and no login parameters.
func plantLegacyCredential(t *testing.T, srv *Server, userID, hash string) {
	t.Helper()
	plantPasswordHash(t, srv, userID, hash)
	usersPath := filepath.Join(srv.configDir, "users.json")
	raw, err := os.ReadFile(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for _, entry := range file["users"].([]any) {
		if e := entry.(map[string]any); e["id"] == userID {
			delete(e, "authDerivation")
			delete(e, "loginSalt")
			delete(e, "loginIterations")
		}
	}
	if raw, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usersPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if u, err := srv.users.Get(userID); err != nil || !u.NeedsPasswordReset() {
		t.Fatalf("planted account is not legacy: %v", err)
	}
}

// TestAdminCreatedAccountsAreStoredDerived: admin create and admin reset hand
// the server a plaintext password; it is stored derived at once, so the user's
// first sign-in sends only the auth secret.
func TestAdminCreatedAccountsAreStoredDerived(t *testing.T) {
	srv := newTestServer(t)
	create := httptest.NewRecorder()
	srv.handleUsersCreate(create, httptest.NewRequest(http.MethodPost, "/api/users",
		strings.NewReader(`{"username":"new-hire","password":"temporary-test-password","role":"user"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	u, err := srv.users.GetByUsername("new-hire")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if !u.UsesDerivedAuth() || u.NeedsPasswordReset() || strings.Contains(create.Body.String(), "needsPasswordReset") {
		t.Fatalf("admin-created account stored legacy: %+v", u.Public())
	}
	if rec := postLogin(t, srv, map[string]any{"username": "new-hire", "authSecret": testAuthSecret(srv, "new-hire", "temporary-test-password")}); rec.Code != http.StatusOK {
		t.Fatalf("first sign-in with the derived secret: %d %s", rec.Code, rec.Body.String())
	}

	plantLegacyCredential(t, srv, u.ID, u.PasswordHash)
	req := httptest.NewRequest(http.MethodPost, "/api/users/"+u.ID+"/reset-password", strings.NewReader(`{"password":"reset-test-password"}`))
	req.SetPathValue("id", u.ID)
	rec := httptest.NewRecorder()
	srv.handleUsersResetPassword(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := srv.users.Get(u.ID); !after.UsesDerivedAuth() || after.NeedsPasswordReset() {
		t.Fatal("admin reset did not convert a legacy account to derived auth")
	}
	if rec := postLogin(t, srv, map[string]any{"username": "new-hire", "authSecret": testAuthSecret(srv, "new-hire", "reset-test-password")}); rec.Code != http.StatusOK {
		t.Fatalf("sign-in after reset: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPasswordChangeCommitsCredentialAndRewrapTogether is the §2.1 regression
// test: the credential and the re-sealed PGP envelope must land in one write.
func TestPasswordChangeCommitsCredentialAndRewrapTogether(t *testing.T) {
	srv := newTestServer(t)
	const oldPw = "correct-horse-battery-staple"
	u, err := srv.users.Create(context.Background(), "rewrap-user", oldPw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := srv.users.ClearMustChangePassword(u.ID); err != nil {
		t.Fatalf("ClearMustChangePassword: %v", err)
	}
	// Give the account a client-protected identity to re-seal.
	if _, err := srv.users.SetPGPIdentityClientProtected(
		u.ID, "FPR", "KEYID", "-----BEGIN PGP PUBLIC KEY BLOCK-----\npub\n-----END PGP PUBLIC KEY BLOCK-----",
		`{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"AAAA","iv":"BBBB","ciphertext":"OLD"}`,
		"generated", "2026-01-01T00:00:00Z", nil,
	); err != nil {
		t.Fatalf("SetPGPIdentityClientProtected: %v", err)
	}

	newSalt := "bmV3LXNhbHQtc2l4dGVlbg=="
	newSecret := "aa" + strings0(62)
	const newEnvelope = `{"v":2,"kdf":"PBKDF2-SHA256","iterations":600000,"salt":"CCCC","iv":"DDDD","ciphertext":"NEW"}`

	body, _ := json.Marshal(map[string]any{
		"oldAuthSecret":   testAuthSecret(srv, "rewrap-user", oldPw),
		"newAuthSecret":   newSecret,
		"newLoginSalt":    newSalt,
		"newIterations":   600000,
		"rewrappedPgpKey": newEnvelope,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleChangePassword(rec, req.WithContext(context.WithValue(req.Context(), authContextKey{},
		AuthContext{UserID: u.ID, Username: u.Username, Role: users.RoleUser})))
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: status %d (%s)", rec.Code, rec.Body.String())
	}

	after, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UsesDerivedAuth() {
		t.Error("account is not on derived auth after the change")
	}
	if after.LoginSalt != newSalt {
		t.Errorf("LoginSalt = %q, want %q", after.LoginSalt, newSalt)
	}
	if ok, _ := users.VerifyAuthSecret(context.Background(), after, newSecret); !ok {
		t.Error("the new derived credential does not verify")
	}
	// The envelope must have moved in the SAME write. If it lags, the key is
	// sealed under a password nobody has any more.
	if after.PGPPrivateKeyWrapped != newEnvelope {
		t.Errorf("PGP envelope = %q, want the rewrapped one — the credential and the envelope must "+
			"commit together or the key is stranded", after.PGPPrivateKeyWrapped)
	}
}

// TestPasswordChangeRejectsPlaintextFields: the retired oldPassword/newPassword
// fields neither prove the current credential nor set a new one.
func TestPasswordChangeRejectsPlaintextFields(t *testing.T) {
	srv := newTestServer(t)
	const oldPw = "correct-horse-battery-staple"
	u, err := srv.users.Create(context.Background(), "mismatch-user", oldPw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	change := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		srv.handleChangePassword(rec, req.WithContext(context.WithValue(req.Context(), authContextKey{},
			AuthContext{UserID: u.ID, Username: u.Username, Role: users.RoleUser})))
		return rec
	}
	if rec := change(map[string]any{"oldPassword": oldPw, "newPassword": "another-correct-horse-battery"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("plaintext oldPassword: status %d, want 401", rec.Code)
	}
	if rec := change(map[string]any{"oldAuthSecret": testAuthSecret(srv, u.Username, oldPw), "newPassword": "another-correct-horse-battery"}); rec.Code != http.StatusBadRequest {
		t.Errorf("plaintext newPassword: status %d, want 400", rec.Code)
	}
	if after, _ := srv.users.Get(u.ID); after.PasswordHash != u.PasswordHash {
		t.Error("a plaintext change body replaced the credential")
	}
}

func strings0(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}

// deriveAuthSecretForTest mirrors frontend/src/lib/authSecret.ts: stretch the
// password with the server-supplied login salt, then HKDF-Expand under the auth
// label. Independent of users.DeriveAuthSecret so the two cross-check.
func deriveAuthSecretForTest(t *testing.T, password, saltB64 string, iterations int) string {
	t.Helper()
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	stretch, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		t.Fatalf("pbkdf2: %v", err)
	}
	// HKDF-Expand with an empty salt and the auth label, one 32-byte block.
	mac := hmac.New(sha256.New, hkdfExtract(nil, stretch))
	mac.Write([]byte("kypost/auth/v1"))
	mac.Write([]byte{0x01})
	return hex.EncodeToString(mac.Sum(nil))
}

func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}
