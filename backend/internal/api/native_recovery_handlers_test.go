package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const recoveryAdminPassword = "operator-test-password"

func nativeRecoveryHTTPFixture(t *testing.T) (*Server, *http.Cookie, sso.NativeRecoveryChallenge) {
	t.Helper()
	t.Setenv("STATE_DIR", t.TempDir())
	srv := newDirectoryTestServer(t)
	settings := srv.ssoStore.Load()
	settings.ClientSecret = strings.Repeat("z", 32)
	if err := srv.ssoStore.Save(settings); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "retained", 1, scimUser("subject", "subject", true)))
	if err := fsutil.PersistJSONFile(filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	actor, err := srv.users.Create(context.Background(), "recovery_operator", recoveryAdminPassword, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err = srv.users.LinkSSO(actor.ID, "operator", "operator", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.users.ClearMustChangePassword(actor.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err = srv.startSession(rec, httptest.NewRequest("GET", "/", nil), actor.ID); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookieFrom(rec)
	sum := sha256.Sum256([]byte(testSyncKey))
	raw, _ := json.Marshal(map[string]any{"password": recoveryAdminPassword, "systemId": "paired-system", "keyFingerprint": hex.EncodeToString(sum[:])})
	rec = gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/challenge", string(raw), "")
	if rec.Code != 200 {
		t.Fatalf("challenge %d: %s", rec.Code, rec.Body.String())
	}
	var response struct{ Challenge sso.NativeRecoveryChallenge }
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return srv, cookie, response.Challenge
}
func recoveryHTTPUpload(t *testing.T, c sso.NativeRecoveryChallenge, size int, key string) string {
	t.Helper()
	now := time.Now().UTC()
	subjects := make([]any, 0, len(c.Subjects))
	for _, sub := range c.Subjects {
		profile := map[string]any{"id": sub, "externalId": sub, "active": false, "roles": []string{}, "displayName": strings.Repeat("x", size)}
		subjects = append(subjects, map[string]any{"id": sub, "revision": 2, "profile": profile})
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "issuer": c.Issuer, "systemId": c.SystemID, "nonce": c.Nonce, "issuedAt": now, "expiresAt": now.Add(5 * time.Minute), "subjects": subjects})
	if err != nil {
		t.Fatal(err)
	}
	headers, err := syncauth.Sign([]byte(key), now, "recovery.evidence", c.Nonce, raw)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, _ := json.Marshal(map[string]any{"password": recoveryAdminPassword, "bodyBase64": base64.StdEncoding.EncodeToString(raw), "signature": headers.Signature, "timestamp": headers.Timestamp, "eventType": headers.EventType, "eventId": headers.EventID})
	return string(wrapper)
}
func TestNativeRecoveryHTTPAcceptsLargeEvidenceAndRetainsHold(t *testing.T) {
	srv, cookie, c := nativeRecoveryHTTPFixture(t)
	body := recoveryHTTPUpload(t, c, 100<<10, testSyncKey)
	if len(body) <= maxActionBytes {
		t.Fatal("fixture does not cross default digest limit")
	}
	rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accountsRepaired":false`) {
		t.Fatalf("upload %d: %s", rec.Code, rec.Body.String())
	}
	if sso.RequireNativeRestoreReleased(srv.stateDir) == nil {
		t.Fatal("hold released")
	}
	u, err := srv.users.GetBySSOSub("subject")
	if err != nil || !u.Active {
		t.Fatal("snapshot profile changed account", err)
	}
	rec = gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, "")
	if rec.Code != 409 {
		t.Fatal("replay accepted", rec.Code)
	}
}
func TestNativeRecoveryHTTPRejectsClientSecretFallback(t *testing.T) {
	srv, cookie, c := nativeRecoveryHTTPFixture(t)
	settings := srv.ssoStore.Load()
	body := recoveryHTTPUpload(t, c, 0, settings.ClientSecret)
	before, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, "")
	after, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	if rec.Code != 409 || !bytes.Equal(before, after) {
		t.Fatal("fallback/changing authority accepted or mutated", rec.Code)
	}
}
func TestNativeRecoveryActionDigestBoundsAndBodyPreservation(t *testing.T) {
	for _, limit := range []int64{maxActionBytes, maxNativeRecoveryUploadBytes} {
		for _, size := range []int{int(limit), int(limit) + 1} {
			called := false
			body := strings.Repeat("x", size)
			rec := httptest.NewRecorder()
			withActionDigestLimit(limit, func(w http.ResponseWriter, r *http.Request) {
				called = true
				got, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != body {
					t.Fatal("body not preserved")
				}
			})(rec, httptest.NewRequest("POST", "/test", strings.NewReader(body)))
			if called != (size <= int(limit)) || size > int(limit) && rec.Code != 413 {
				t.Fatal("digest bound", limit, size, rec.Code)
			}
		}
	}
}

func TestNativeRecoveryLargeSSOStepUpBindsExactReplay(t *testing.T) {
	srv, cookie, c := nativeRecoveryHTTPFixture(t)
	srv.sessMu.Lock()
	sess := srv.sessions[cookie.Value]
	sess.SSOKySignOn = true
	sess.SSOAppAdmin = true
	sess.SSO = sso.SessionIdentity{Issuer: c.Issuer, ClientID: "kypost", Subject: "operator"}
	srv.sessions[cookie.Value] = sess
	srv.sessMu.Unlock()
	body := recoveryHTTPUpload(t, c, 100<<10, testSyncKey)
	grant := challengeFrom(t, gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, ""))
	srv.stepUpMu.Lock()
	proof := srv.stepUps[grant]
	proof.verified = true
	srv.stepUps[grant] = proof
	srv.stepUpMu.Unlock()
	altered := strings.Replace(body, `"password":"operator-test-password"`, `"password":"different"`, 1)
	if altered == body {
		t.Fatal("fixture not changed")
	}
	rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", altered, grant)
	if rec.Code != 403 {
		t.Fatal("altered replay accepted", rec.Code)
	}
	rec = gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, grant)
	if rec.Code != 200 {
		t.Fatalf("exact replay %d: %s", rec.Code, rec.Body.String())
	}
}
func TestNativeRecoveryRefusesAdminCycleAfterStepUp(t *testing.T) {
	srv, cookie, c := nativeRecoveryHTTPFixture(t)
	srv.sessMu.Lock()
	sess := srv.sessions[cookie.Value]
	sess.SSOKySignOn = true
	sess.SSOAppAdmin = true
	sess.SSO = sso.SessionIdentity{Issuer: c.Issuer, ClientID: "kypost", Subject: "operator"}
	srv.sessions[cookie.Value] = sess
	srv.sessMu.Unlock()
	body := recoveryHTTPUpload(t, c, 0, testSyncKey)
	grant := challengeFrom(t, gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, ""))
	srv.stepUpMu.Lock()
	proof := srv.stepUps[grant]
	proof.verified = true
	srv.stepUps[grant] = proof
	srv.stepUpMu.Unlock()
	release, err := fsutil.LockFile(filepath.Join(srv.configDir, "sso.json"))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", body, grant) }()
	deadline := time.Now().Add(time.Second)
	for {
		srv.stepUpMu.Lock()
		_, pending := srv.stepUps[grant]
		srv.stepUpMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("step-up not consumed before fence")
		}
		time.Sleep(time.Millisecond)
	}
	other, err := users.OpenExisting(context.Background(), srv.configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Deactivate(sess.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = other.Reactivate(sess.UserID); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	release()
	released = true
	select {
	case rec := <-done:
		if rec.Code != 409 {
			t.Fatalf("admin cycle admitted: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
	after, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("stale admin proof committed evidence")
	}
}

func TestNativeRecoveryChallengeRefusesStaleSSOAuthority(t *testing.T) {
	cases := map[string]func(*Server, Session) error{
		"client": func(s *Server, _ Session) error {
			settings := s.ssoStore.Load()
			settings.ClientID = "replacement-client"
			return s.ssoStore.Save(settings)
		},
		"issuer": func(s *Server, _ Session) error {
			settings := s.ssoStore.Load()
			settings.IssuerURL = "https://replacement.example"
			return s.ssoStore.Save(settings)
		},
		"revoked link": func(s *Server, sess Session) error { return s.users.RevokeSSOLink(sess.UserID) },
		"changed subject": func(s *Server, sess Session) error {
			return s.users.LinkSSO(sess.UserID, "operator-new", "operator", "")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv, cookie, c := nativeRecoveryHTTPFixture(t)
			srv.sessMu.Lock()
			sess := srv.sessions[cookie.Value]
			sess.SSOKySignOn = true
			sess.SSOAppAdmin = true
			sess.SSO = sso.SessionIdentity{Issuer: c.Issuer, ClientID: "kypost", Subject: "operator"}
			srv.sessions[cookie.Value] = sess
			srv.sessMu.Unlock()
			raw, _ := json.Marshal(map[string]any{"systemId": c.SystemID, "keyFingerprint": c.KeyFingerprint})
			body := string(raw)
			grant := challengeFrom(t, gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/challenge", body, ""))
			srv.stepUpMu.Lock()
			proof := srv.stepUps[grant]
			proof.verified = true
			srv.stepUps[grant] = proof
			srv.stepUpMu.Unlock()
			if err := mutate(srv, sess); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
			rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/challenge", body, grant)
			after, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
			if rec.Code == 200 || rec.Code == 500 || !bytes.Equal(before, after) {
				t.Fatalf("stale SSO authority accepted/mutated: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestNativeRecoveryHTTPDecodedEvidenceBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(map[int]string{0: "maximum", 1: "overflow"}[extra], func(t *testing.T) {
			srv, cookie, c := nativeRecoveryHTTPFixture(t)
			var wrapper map[string]string
			if err := json.Unmarshal([]byte(recoveryHTTPUpload(t, c, 0, testSyncKey)), &wrapper); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(wrapper["bodyBase64"])
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, bytes.Repeat([]byte(" "), sso.MaxNativeRecoveryEvidenceBytes+extra-len(raw))...)
			at, err := time.Parse(time.RFC3339, wrapper["timestamp"])
			if err != nil {
				t.Fatal(err)
			}
			headers, err := syncauth.Sign([]byte(testSyncKey), at, "recovery.evidence", c.Nonce, raw)
			if err != nil {
				t.Fatal(err)
			}
			wrapper["bodyBase64"] = base64.StdEncoding.EncodeToString(raw)
			wrapper["signature"] = headers.Signature
			encoded, _ := json.Marshal(wrapper)
			before, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
			rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", string(encoded), "")
			if extra == 0 && rec.Code != 200 {
				t.Fatalf("maximum decoded evidence %d: %s", rec.Code, rec.Body.String())
			}
			if extra == 1 {
				after, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
				if rec.Code != 400 || !bytes.Equal(before, after) {
					t.Fatal("decoded overflow accepted or mutated lifecycle", rec.Code)
				}
			}
		})
	}
}

func TestNativeRecoveryStatusIsAdminOnlyAndLeaksNoEvidence(t *testing.T) {
	srv, cookie, c := nativeRecoveryHTTPFixture(t)
	if rec := gatedCall(t, srv, cookie, "POST", "/api/admin/native-recovery/evidence", recoveryHTTPUpload(t, c, 64, testSyncKey), ""); rec.Code != 200 {
		t.Fatalf("upload %d: %s", rec.Code, rec.Body.String())
	}
	before, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	rec := gatedCall(t, srv, cookie, "GET", "/api/admin/native-recovery/status", "", "")
	after, _ := os.ReadFile(filepath.Join(srv.configDir, "sso-lifecycle.json"))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(before, after) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var st sso.NativeRestoreReleaseStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Held || st.Epoch != c.Epoch || len(st.Preconditions) != 9 || st.Preconditions[0].OK || st.Preconditions[0].Reasons[0] != "recovery evidence is recorded but repair has not run" {
		t.Fatalf("status %+v", st)
	}
	for _, secret := range []string{c.Nonce, c.AuthorityDigest, c.KeyFingerprint, c.SystemID, "xxxxxxxx", "signature", testSyncKey} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("status leaks %q: %s", secret, rec.Body.String())
		}
	}
	plain, err := srv.users.Create(context.Background(), "plain_user", recoveryAdminPassword, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = srv.users.ClearMustChangePassword(plain.ID); err != nil {
		t.Fatal(err)
	}
	session := httptest.NewRecorder()
	if err = srv.startSession(session, httptest.NewRequest("GET", "/", nil), plain.ID); err != nil {
		t.Fatal(err)
	}
	if rec := gatedCall(t, srv, sessionCookieFrom(session), "GET", "/api/admin/native-recovery/status", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status %d: %s", rec.Code, rec.Body.String())
	}
}
