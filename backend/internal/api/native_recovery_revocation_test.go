package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeRecoveryPublishedNativeRevocation(t *testing.T) {
	for _, scenario := range []string{"offboarding", "demotion", "storage-failure"} {
		t.Run(scenario, func(t *testing.T) {
			demote := scenario == "demotion"
			t.Setenv("STATE_DIR", t.TempDir())
			s := newNativeRuntimeServer(t)
			resource := runtimeDirectoryUser(true)
			resource["roles"] = []string{"kypost.admin"}
			directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "native-create", 1, resource))
			native, e := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
			if e != nil || native.NativeMailboxSource == "" {
				t.Fatal(e)
			}
			if _, e = s.users.SetPassword(context.Background(), native.ID, "retained-native-password", false, nil); e != nil {
				t.Fatal(e)
			}
			actor, e := s.users.Create(context.Background(), "recovery-review-admin", recoveryAdminPassword, users.RoleAdmin)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.users.ClearMustChangePassword(actor.ID); e != nil {
				t.Fatal(e)
			}
			store, e := s.userStore(native.ID)
			if e != nil {
				t.Fatal(e)
			}
			previousSubscriber, e := store.GetOrCreateSubscriberID()
			if e != nil {
				t.Fatal(e)
			}
			if e = store.UpsertNativeDevice(state.NativeDevice{DeviceID: "historical-device", SecretHash: users.HashDeviceSecret("historical-secret")}); e != nil {
				t.Fatal(e)
			}

			legacy, e := s.users.CreateSSOUser("legacy-mixed", users.RoleUser, "legacy-subject", "legacy", "")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.userStore(legacy.ID); e != nil {
				t.Fatal(e)
			}
			hash, e := users.HashPassword(context.Background(), "legacy-dav-password")
			if e != nil {
				t.Fatal(e)
			}
			if e = s.writeDAVPassword(legacy.ID, davPasswordFile{Hash: hash}); e != nil {
				t.Fatal(e)
			}
			checkDAV := func(want int) {
				t.Helper()
				req := httptest.NewRequest("PROPFIND", "/dav/legacy-mixed/", nil)
				req.SetBasicAuth(legacy.Username, "legacy-dav-password")
				rec := httptest.NewRecorder()
				s.withDAVBasicAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(rec, req)
				if rec.Code != want {
					t.Fatal("legacy DAV", rec.Code, rec.Body.String())
				}
			}

			// Obtain a real password session before restore quarantine.
			w := httptest.NewRecorder()
			login, _ := json.Marshal(map[string]any{"username": native.Username, "password": "retained-native-password"})
			lr := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(login)))
			lr.Header.Set("Content-Type", "application/json")
			s.routes().ServeHTTP(w, lr)
			if w.Code != 200 {
				t.Fatal("native login before loss", w.Code, w.Body.String())
			}
			nativeCookie := sessionCookieFrom(w)

			if e = fsutil.PersistJSONFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); e != nil {
				t.Fatal(e)
			}
			w = httptest.NewRecorder()
			if e = s.startSession(w, httptest.NewRequest("GET", "/", nil), actor.ID); e != nil {
				t.Fatal(e)
			}
			cookie := sessionCookieFrom(w)
			sum := sha256.Sum256([]byte(testSyncKey))
			b, _ := json.Marshal(map[string]any{"password": recoveryAdminPassword, "systemId": "paired-system", "keyFingerprint": hex.EncodeToString(sum[:])})
			w = gatedCall(t, s, cookie, "POST", "/api/admin/native-recovery/challenge", string(b), "")
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var response struct{ Challenge sso.NativeRecoveryChallenge }
			if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil {
				t.Fatal(e)
			}
			c := response.Challenge
			w = gatedCall(t, s, cookie, "POST", "/api/admin/native-recovery/evidence", recoveryHTTPUpload(t, c, 0, testSyncKey), "")
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			checkDAV(204) // Legacy mail authority is usable while native mail is held.

			lossResource := runtimeDirectoryUser(demote)
			lossResource["roles"] = []string{}
			if scenario == "storage-failure" {
				path := filepath.Join(s.userStateDir(native.ID), "state.db")
				if e = os.Rename(path, path+".saved"); e != nil {
					t.Fatal(e)
				}
				failed := postDirectory(t, s, testSyncKey, "user.updated", "native-loss", 2, lossResource)
				if failed.Code != 500 {
					t.Fatal("missing state did not refuse revocation", failed.Code)
				}
				if _, e = os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("missing native state recreated", e)
				}
				d, _, e := s.ssoLifecycle.Directory("https://idp.example", native.SSOSub)
				if e != nil || d.Revision != 1 {
					t.Fatal("failed revocation acknowledged event", e)
				}
				if e = os.Rename(path+".saved", path); e != nil {
					t.Fatal(e)
				}
			}
			loss := postDirectory(t, s, testSyncKey, "user.updated", "native-loss", 2, lossResource)
			if loss.Code != 200 {
				t.Fatal("signed queued loss refused", loss.Code, loss.Body.String())
			}
			current, e := s.users.Get(native.ID)
			if e != nil {
				t.Fatal(e)
			}
			if current.Active != demote || demote && current.Role != users.RoleUser {
				t.Fatal("queued loss did not revoke account authority", current.Active, current.Role)
			}
			admin := gatedCall(t, s, nativeCookie, "GET", "/api/admin/mail-domain", "", "")
			if admin.Code != 401 {
				t.Fatal("existing admin session survived loss", admin.Code, admin.Body.String())
			}
			w = httptest.NewRecorder()
			lr = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(login)))
			lr.Header.Set("Content-Type", "application/json")
			s.routes().ServeHTTP(w, lr)
			if w.Code != 401 || sessionCookieFrom(w) != nil {
				t.Fatal("held native local login succeeded after loss", w.Code, w.Body.String())
			}

			if !demote {
				devices, e := store.ListNativeDevicesStrict()
				if e != nil || len(devices) != 0 {
					t.Fatal("held devices not revoked", e)
				}
				if store.SubscriberID() == previousSubscriber {
					t.Fatal("held pairing generation not rotated")
				}
			}
			if _, e := s.userStore(native.ID); !errors.Is(e, sso.ErrNativeRestoreHold) {
				t.Fatal("cached ordinary state admitted behind hold", e)
			}
			assertRecoveryReceiptInvalidated(t, s)
			legacyLoss := postDirectory(t, s, testSyncKey, "user.updated", "legacy-loss", 2, scimUser(legacy.SSOSub, legacy.Username, false))
			if legacyLoss.Code != 200 {
				t.Fatal("mixed legacy offboarding", legacyLoss.Code, legacyLoss.Body.String())
			}
			checkDAV(401)
			if _, e = os.Stat(s.userCardDAVAuthPath(legacy.ID)); !os.IsNotExist(e) {
				t.Fatal("legacy DAV hash retained", e)
			}
			if sso.RequireNativeRestoreReleased(s.stateDir) == nil {
				t.Fatal("hold released")
			}
		})
	}
}
func assertRecoveryReceiptInvalidated(t *testing.T, s *Server) {
	t.Helper()
	body, e := os.ReadFile(filepath.Join(s.configDir, "sso-lifecycle.json"))
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		RecoveryReceipt   json.RawMessage `json:"recoveryReceipt"`
		RecoveryChallenge json.RawMessage `json:"recoveryChallenge"`
	}
	if e = json.Unmarshal(body, &f); e != nil {
		t.Fatal(e)
	}
	if len(f.RecoveryReceipt) != 0 || len(f.RecoveryChallenge) != 0 {
		t.Fatal("ordinary loss retained evidence qualification")
	}
}

func TestNativeRecoveryPublishedLegacyOffboarding(t *testing.T) {
	s, c, ch := nativeRecoveryHTTPFixture(t)
	rec := gatedCall(t, s, c, "POST", "/api/admin/native-recovery/evidence", recoveryHTTPUpload(t, ch, 0, testSyncKey), "")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	response := postDirectory(t, s, testSyncKey, "user.updated", "queued-loss", 2, scimUser("subject", "subject", false))
	u, e := s.users.GetBySSOSub("subject")
	if e != nil {
		t.Fatal(e)
	}
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	assertRecoveryReceiptInvalidated(t, s)
	if u.Active {
		t.Fatalf("signed offboarding blocked: status=%d active=%v response=%s", response.Code, u.Active, strings.TrimSpace(response.Body.String()))
	}
}
