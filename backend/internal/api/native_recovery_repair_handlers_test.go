//go:build linux

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const nativeRepairPath = "/api/admin/native-recovery/repair"
const nativeRepairBody = `{"password":"operator-test-password"}`

type nativeRepairFixture struct {
	s                    *Server
	cookie, nativeCookie *http.Cookie
	native               users.User
	subscriber           string
	box                  *mailbox.Store
	message              int64
	raw                  []byte
}

func nativeRepairHTTPFixture(t *testing.T, scenario string, revoked bool) nativeRepairFixture {
	t.Helper()
	t.Setenv("STATE_DIR", t.TempDir())
	s := newNativeRuntimeServer(t)
	legacyMixedUseAdmin(t, s, runtimeDirectoryUser(true))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.SetPassword(context.Background(), u.ID, "retained-native-password", false, nil); err != nil {
		t.Fatal(err)
	}
	identity, err := pgpmail.GenerateIdentity("Retained", "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// Structurally accepted opaque client envelope; no client decryption claimed.
	if _, err = s.users.SetPGPIdentityClientProtected(u.ID, info.Fingerprint, info.KeyID, info.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil); err != nil {
		t.Fatal(err)
	}
	u, err = s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := s.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertNativeDevice(state.NativeDevice{DeviceID: "historical-device", SecretHash: users.HashDeviceSecret("historical-secret")}); err != nil {
		t.Fatal(err)
	}
	hash, err := users.HashPassword(context.Background(), "historical-dav-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeDAVPassword(u.ID, davPasswordFile{Hash: hash}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: retained mail\r\n\r\nretained bytes")
	message, err := box.Append(context.Background(), "INBOX", bytes.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	client, err := s.userMailClient(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.ApplyLabel(context.Background(), strconv.FormatInt(message, 10), "Retained"); err != nil {
		t.Fatal(err)
	}
	w := doJSON(s, s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": u.Username, "password": "retained-native-password"})
	if w.Code != 200 {
		t.Fatal("native pre-hold login", w.Code)
	}
	nativeCookie := sessionCookieFrom(w)
	if revoked {
		if err = s.users.RevokeSSOLink(u.ID); err != nil {
			t.Fatal(err)
		}
	}
	u, err = s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.users.Create(context.Background(), "recovery-operator", recoveryAdminPassword, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.ClearMustChangePassword(actor.ID); err != nil {
		t.Fatal(err)
	}
	// Release takes only an unlinked local administrator (P6).
	if scenario != "release" {
		if err = s.users.LinkSSO(actor.ID, "operator", "operator", ""); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "reactivated-admin" {
		if _, err = s.users.SetRole(u.ID, users.RoleUser); err != nil {
			t.Fatal(err)
		}
		if u, err = s.users.Deactivate(u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = fsutil.PersistJSONFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	if scenario == "release" {
		qualifyReleaseFixture(t, s, u)
	}
	w = httptest.NewRecorder()
	if err = s.startSession(w, httptest.NewRequest("GET", "/", nil), actor.ID); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookieFrom(w)
	fingerprint := sha256.Sum256([]byte(testSyncKey))
	b, _ := json.Marshal(map[string]any{"password": recoveryAdminPassword, "systemId": "paired-system", "keyFingerprint": hex.EncodeToString(fingerprint[:])})
	w = gatedCall(t, s, cookie, "POST", "/api/admin/native-recovery/challenge", string(b), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct{ Challenge sso.NativeRecoveryChallenge }
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	c := response.Challenge
	var subjects []any
	for _, subject := range c.Subjects {
		profile := map[string]any{"id": subject, "externalId": subject, "active": false, "roles": []string{}}
		if subject == u.SSOSub && (scenario == "demoted" || scenario == "active-admin" || scenario == "reactivated-admin") {
			profile["active"] = true
			profile["userName"] = u.SSOUsername
			if scenario == "active-admin" || scenario == "reactivated-admin" {
				profile["roles"] = []string{"kypost.admin"}
			}
		}
		subjects = append(subjects, map[string]any{"id": subject, "revision": 2, "profile": profile})
	}
	now := time.Now().UTC()
	rawEvidence, _ := json.Marshal(map[string]any{"version": 1, "issuer": c.Issuer, "systemId": c.SystemID, "nonce": c.Nonce, "issuedAt": now, "expiresAt": now.Add(5 * time.Minute), "subjects": subjects})
	headers, err := syncauth.Sign([]byte(testSyncKey), now, "recovery.evidence", c.Nonce, rawEvidence)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(map[string]any{"password": recoveryAdminPassword, "bodyBase64": base64.StdEncoding.EncodeToString(rawEvidence), "signature": headers.Signature, "timestamp": headers.Timestamp, "eventType": headers.EventType, "eventId": headers.EventID})
	w = gatedCall(t, s, cookie, "POST", "/api/admin/native-recovery/evidence", string(b), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return nativeRepairFixture{s, cookie, nativeCookie, u, subscriber, box, message, raw}
}

func repairLifecycle(t *testing.T, s *Server) (bool, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.configDir, "sso-lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		RecoveryRepair *struct{ CompletedAt *time.Time } `json:"recoveryRepair"`
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.RecoveryRepair != nil, f.RecoveryRepair != nil && f.RecoveryRepair.CompletedAt != nil
}

func TestNativeRecoveryRepairHTTPReconcilesAndRevokesWhileHeld(t *testing.T) {
	for _, scenario := range []string{"inactive", "demoted", "already-revoked", "active-admin", "reactivated-admin"} {
		t.Run(scenario, func(t *testing.T) {
			f := nativeRepairHTTPFixture(t, scenario, scenario == "already-revoked")
			before, err := f.s.users.List()
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := f.box.List(context.Background(), "INBOX", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			w := gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"accountsRepaired":true`) || !strings.Contains(w.Body.String(), `"restoreHeld":true`) {
				t.Fatal(w.Code, w.Body.String())
			}
			intent, complete := repairLifecycle(t, f.s)
			if !intent || !complete {
				t.Fatal("cleanup did not complete journal")
			}
			for _, old := range before {
				after, err := f.s.users.Get(old.ID)
				if err != nil {
					t.Fatal(err)
				}
				if old.ID == f.native.ID {
					expectedRole, expectedEpoch := users.RoleUser, old.NativeSendEpoch+1
					if scenario == "reactivated-admin" {
						expectedRole = users.RoleAdmin
					}
					if scenario == "active-admin" {
						expectedRole, expectedEpoch = users.RoleAdmin, old.NativeSendEpoch
					}
					if after.Active != (scenario == "demoted" || scenario == "active-admin" || scenario == "reactivated-admin") || after.Role != expectedRole || after.NativeSendEpoch != expectedEpoch || (after.Active && after.DeactivatedAt != "") {
						t.Fatal("wrong access/epoch")
					}
					old.Active, old.Role, old.NativeSendEpoch, old.UpdatedAt, old.DeactivatedAt = after.Active, after.Role, after.NativeSendEpoch, after.UpdatedAt, after.DeactivatedAt
				}
				if !reflect.DeepEqual(old, after) {
					t.Fatal("changed legacy/credentials/PGP/link flags")
				}
			}
			store, err := state.OpenNative(f.s.userStateDir(f.native.ID), f.native.NativeMailboxSource)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			devices, err := store.ListNativeDevicesStrict()
			if err != nil || len(devices) != 0 {
				t.Fatal("devices survived cleanup", err)
			}
			subscriber, err := store.GetOrCreateSubscriberID()
			if err != nil || subscriber == f.subscriber {
				t.Fatal("pairing authority survived", err)
			}
			if _, err = os.Stat(f.s.userCardDAVAuthPath(f.native.ID)); !os.IsNotExist(err) {
				t.Fatal("DAV credential survived", err)
			}
			currentRaw, err := f.box.Raw(context.Background(), "INBOX", f.message)
			if err != nil || !bytes.Equal(f.raw, currentRaw) {
				t.Fatal("mail changed", err)
			}
			currentMetadata, err := f.box.List(context.Background(), "INBOX", 0, 10)
			if err != nil || !reflect.DeepEqual(metadata, currentMetadata) {
				t.Fatal("labels/mail metadata changed", err)
			}
			f.s.sessMu.RLock()
			_, historicalSessionExists := f.s.sessions[f.nativeCookie.Value]
			f.s.sessMu.RUnlock()
			if historicalSessionExists {
				t.Fatal("historical session retained")
			}
			if w = gatedCall(t, f.s, f.nativeCookie, "GET", "/api/admin/mail-domain", "", ""); w.Code != 401 {
				t.Fatal("native old session usable", w.Code)
			}
			if w = doJSON(f.s, f.s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": f.native.Username, "password": "retained-native-password"}); w.Code != 401 {
				t.Fatal("native password usable", w.Code)
			}
			if sso.RequireNativeRestoreReleased(f.s.stateDir) == nil {
				t.Fatal("hold released")
			}
			if w = gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, ""); w.Code != 409 {
				t.Fatal("repair repeated", w.Code)
			}
		})
	}
}

func TestNativeRecoveryRepairHTTPFailsClosed(t *testing.T) {
	for _, scenario := range []string{"bad-password", "csrf", "missing-storage", "cleanup-failure", "changed-key"} {
		t.Run(scenario, func(t *testing.T) {
			f := nativeRepairHTTPFixture(t, "inactive", false)
			before, err := f.s.users.Get(f.native.ID)
			if err != nil {
				t.Fatal(err)
			}
			body := nativeRepairBody
			switch scenario {
			case "bad-password":
				body = `{"password":"wrong"}`
			case "missing-storage":
				if err = os.Remove(filepath.Join(f.s.userStateDir(f.native.ID), "state.db")); err != nil {
					t.Fatal(err)
				}
			case "cleanup-failure":
				p := f.s.userCardDAVAuthPath(f.native.ID)
				if err = os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(p, "blocked"), []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-key":
				f.s.pairingSecret = strings.Repeat("x", 32)
			}
			var w *httptest.ResponseRecorder
			if scenario == "csrf" {
				r := httptest.NewRequest("POST", nativeRepairPath, strings.NewReader(body))
				r.AddCookie(f.cookie)
				r.Header.Set("Content-Type", "application/json")
				w = httptest.NewRecorder()
				f.s.routes().ServeHTTP(w, r)
			} else {
				w = gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, body, "")
			}
			if w.Code == 200 {
				t.Fatal("unsafe repair accepted")
			}
			intent, complete := repairLifecycle(t, f.s)
			if complete || intent != (scenario == "cleanup-failure") {
				t.Fatal("incorrect partial repair qualification")
			}
			after, err := f.s.users.Get(f.native.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cleanup-failure" {
				if after.Active || after.Role != users.RoleUser {
					t.Fatal("accounts not repaired before cleanup failure")
				}
			} else if !reflect.DeepEqual(before, after) {
				t.Fatal("precondition failure changed accounts")
			}
			if sso.RequireNativeRestoreReleased(f.s.stateDir) == nil {
				t.Fatal("failed repair released hold")
			}
		})
	}
}

func nativeRepairSSOSession(f nativeRepairFixture) {
	settings := f.s.ssoStore.Load()
	f.s.sessMu.Lock()
	session := f.s.sessions[f.cookie.Value]
	session.SSOKySignOn = true
	session.SSOAppAdmin = true
	session.SSO = sso.SessionIdentity{Issuer: settings.IssuerURL, ClientID: "kypost", Subject: "operator"}
	f.s.sessions[f.cookie.Value] = session
	f.s.sessMu.Unlock()
}

func TestNativeRecoveryRepairHTTPConsumesStepUpOnlyOnce(t *testing.T) {
	f := nativeRepairHTTPFixture(t, "inactive", false)
	nativeRepairSSOSession(f)
	grant := challengeFrom(t, gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, ""))
	// Exercise action-bound single-use confirmation; provider proof is synthetic.
	f.s.stepUpMu.Lock()
	proof := f.s.stepUps[grant]
	proof.verified = true
	f.s.stepUps[grant] = proof
	f.s.stepUpMu.Unlock()
	w := gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, grant)
	if w.Code != 200 {
		t.Fatal("confirmation repeated or witnesses lost", w.Code, w.Body.String())
	}
}

func TestNativeRecoveryRepairHTTPRechecksConfirmedActor(t *testing.T) {
	for _, change := range []string{"account-cycle", "session-revocation"} {
		t.Run(change, func(t *testing.T) {
			f := nativeRepairHTTPFixture(t, "inactive", false)
			nativeRepairSSOSession(f)
			grant := challengeFrom(t, gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, ""))
			f.s.stepUpMu.Lock()
			proof := f.s.stepUps[grant]
			proof.verified = true
			f.s.stepUps[grant] = proof
			f.s.stepUpMu.Unlock()
			release, err := fsutil.LockFile(filepath.Join(f.s.configDir, "sso.json"))
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
			go func() { done <- gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, grant) }()
			deadline := time.Now().Add(time.Second)
			for {
				f.s.stepUpMu.Lock()
				_, pending := f.s.stepUps[grant]
				f.s.stepUpMu.Unlock()
				if !pending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("confirmation never consumed")
				}
				time.Sleep(time.Millisecond)
			}
			actor, err := f.s.users.GetByUsername("recovery-operator")
			if err != nil {
				t.Fatal(err)
			}
			if change == "account-cycle" {
				other, err := users.OpenExisting(context.Background(), f.s.configDir)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = other.Deactivate(actor.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = other.Reactivate(actor.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.s.revokeUserSessions(actor.ID, "")
			}
			before, _ := os.ReadFile(filepath.Join(f.s.configDir, "sso-lifecycle.json"))
			release()
			released = true
			select {
			case w := <-done:
				if w.Code != 409 {
					t.Fatal("stale operator proof admitted", w.Code, w.Body.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stale request did not finish")
			}
			after, _ := os.ReadFile(filepath.Join(f.s.configDir, "sso-lifecycle.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("stale proof changed recovery state")
			}
			u, err := f.s.users.Get(f.native.ID)
			if err != nil || !u.Active || u.Role != users.RoleAdmin {
				t.Fatal("stale proof repaired account", err)
			}
		})
	}
}
