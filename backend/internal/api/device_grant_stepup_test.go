package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// A session alone must not turn itself into a device credential or a second
// factor: pairing, push 2FA and the approver flag all take step-up.

const deviceGrantPassword = "pw-grant-testpassword"

func newDeviceGrantServer(t *testing.T) (*Server, users.User) {
	t.Helper()
	srv := newTestServer(t)
	u, err := srv.users.Create(context.Background(), "grant", deviceGrantPassword, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	return srv, u
}

func deviceGrantCall(t *testing.T, srv *Server, userID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// pairingStepUpBody gives userID a known password and returns the body that
// re-proves it, for tests that need a pairing token.
func pairingStepUpBody(t *testing.T, srv *Server, userID string) string {
	t.Helper()
	const password = "pairing-stepup-testpassword"
	if _, err := srv.users.SetPassword(context.Background(), userID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	return `{"authSecret":"` + userAuthSecret(srv, userID, password) + `"}`
}

func TestPairingTokenRequiresStepUp(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	srv.pairingSecret = "test-pairing-secret-at-least-32-bytes-long"
	srv.serverBaseURL = "https://mail.example"

	for name, body := range map[string]string{"no credential": `{}`, "wrong credential": `{"authSecret":"wrong-secret"}`} {
		rec := deviceGrantCall(t, srv, u.ID, http.MethodPost, "/api/notifications/pairing", body)
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "pairingToken") {
			t.Fatalf("%s: status %d body %s, want 403 and no token", name, rec.Code, rec.Body.String())
		}
	}

	rec := deviceGrantCall(t, srv, u.ID, http.MethodPost, "/api/notifications/pairing", `{"authSecret":"`+userAuthSecret(srv, u.ID, deviceGrantPassword)+`"}`)
	var out struct {
		DeepLink string `json:"deepLink"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.DeepLink == "" {
		t.Fatalf("stepped-up pairing: status %d body %s", rec.Code, rec.Body.String())
	}

	// The app side is unchanged: the token registers a device, which is not an approver.
	if reg := registerWithPairingToken(t, srv, out.DeepLink); reg.Code != http.StatusOK {
		t.Fatalf("register: status %d body %s", reg.Code, reg.Body.String())
	}
	store, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := store.GetNativeDevice("device-signon"); !ok || d.MFAApprover {
		t.Fatalf("new device = %+v (found %v), want a non-approver", d, ok)
	}
}

func TestPushMFAEnableRequiresStepUp(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	enrollTOTP(t, srv, u.ID)
	pairNativeDevice(t, srv, u.ID, "dev-stepup")

	rec := deviceGrantCall(t, srv, u.ID, http.MethodPut, "/api/mfa/push/enabled", `{"enabled":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("session-only enable: status %d body %s, want 403", rec.Code, rec.Body.String())
	}
	if got, _ := srv.users.Get(u.ID); got.PushMFAEnabled {
		t.Fatal("session-only request enabled push 2FA")
	}

	rec = deviceGrantCall(t, srv, u.ID, http.MethodPut, "/api/mfa/push/enabled", `{"enabled":true,"authSecret":"`+userAuthSecret(srv, u.ID, deviceGrantPassword)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stepped-up enable: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := srv.users.Get(u.ID); !got.PushMFAEnabled {
		t.Fatal("stepped-up request did not enable push 2FA")
	}
}

func TestDeviceApproverFlagRequiresStepUp(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	pairNativeDevice(t, srv, u.ID, "dev-flag")
	store, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetNativeDeviceMFAApprover("dev-flag", false); err != nil {
		t.Fatal(err)
	}
	path := "/api/notifications/native/devices/dev-flag/mfa"

	rec := deviceGrantCall(t, srv, u.ID, http.MethodPut, path, `{"approver":true}`)
	if d, _ := store.GetNativeDevice("dev-flag"); rec.Code != http.StatusForbidden || d.MFAApprover {
		t.Fatalf("session-only set: status %d approver %v, want 403 and unchanged", rec.Code, d.MFAApprover)
	}

	rec = deviceGrantCall(t, srv, u.ID, http.MethodPut, path, `{"approver":true,"authSecret":"`+userAuthSecret(srv, u.ID, deviceGrantPassword)+`"}`)
	if d, _ := store.GetNativeDevice("dev-flag"); rec.Code != http.StatusOK || !d.MFAApprover {
		t.Fatalf("stepped-up set: status %d approver %v body %s", rec.Code, d.MFAApprover, rec.Body.String())
	}

	rec = deviceGrantCall(t, srv, u.ID, http.MethodPut, path, `{"approver":false}`)
	if d, _ := store.GetNativeDevice("dev-flag"); rec.Code != http.StatusForbidden || !d.MFAApprover {
		t.Fatalf("session-only clear: status %d approver %v, want 403 and unchanged", rec.Code, d.MFAApprover)
	}
}

// With no device flagged, no device approves: the old "none flagged means
// all" fallback made every freshly paired device an approver.
func TestUnflaggedDeviceCannotApprove(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	enrollTOTP(t, srv, u.ID)
	deviceID, secret := pairNativeDevice(t, srv, u.ID, "dev-unflagged")
	store, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetNativeDeviceMFAApprover(deviceID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.SetPushMFAEnabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if eligible, err := mfaApproverDevices(store); err != nil || len(eligible) != 0 {
		t.Fatalf("eligible approvers = %v (%v), want none", eligible, err)
	}
	ch, err := srv.mfaChallenges.Create(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := respondPush(srv, ch.ID, deviceID, secret, true); rec.Code != http.StatusForbidden {
		t.Fatalf("unflagged device approval: status %d body %s, want 403", rec.Code, rec.Body.String())
	}
}

// A KySignOn session gets an action-bound challenge, which also proves each
// route carries withActionDigest (without it the gate answers 500).
func TestDeviceGrantsChallengeKySignOnSessions(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	clearMustChangePassword(t, srv, u.ID)
	srv.sessMu.Lock()
	srv.sessions["sso-grant"] = Session{UserID: u.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true}
	srv.sessMu.Unlock()
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/notifications/pairing", `{}`},
		{http.MethodPut, "/api/mfa/push/enabled", `{"enabled":false}`},
		{http.MethodPut, "/api/notifications/native/devices/any/mfa", `{"approver":true}`},
	} {
		req := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "sso-csrf")
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: "sso-grant"})
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		challengeFrom(t, rec)
	}
}

// Newly merged device grants keep the authSecret-only wire contract.
func TestDeviceGrantsRejectAccountPassword(t *testing.T) {
	srv, u := newDeviceGrantServer(t)
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/notifications/pairing", `{"password":"` + deviceGrantPassword + `"}`},
		{http.MethodPut, "/api/mfa/push/enabled", `{"enabled":false,"password":"` + deviceGrantPassword + `"}`},
		{http.MethodPut, "/api/notifications/native/devices/any/mfa", `{"approver":true,"password":"` + deviceGrantPassword + `"}`},
	} {
		rec := deviceGrantCall(t, srv, u.ID, call.method, call.path, call.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s accepted plaintext credential: status %d body %s", call.path, rec.Code, rec.Body.String())
		}
	}
}
