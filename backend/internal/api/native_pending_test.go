package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Device approval (#516): a device that redeems a web-minted pairing token is
// pending and has no access until the signed-in owner approves it.

type pendingRegistration struct {
	OK             bool   `json:"ok"`
	Synced         bool   `json:"synced"`
	Status         string `json:"status"`
	DeviceID       string `json:"deviceId"`
	DeviceSecret   string `json:"deviceSecret"`
	ApprovalCode   string `json:"approvalCode"`
	StatusEndpoint string `json:"statusEndpoint"`
}

func newPendingServer(t *testing.T) (*Server, users.User) {
	t.Helper()
	srv, u := newDeviceGrantServer(t)
	srv.serverBaseURL = "https://mail.example"
	srv.pinProbeHost, srv.pinProbeFailedAt = srv.serverBaseURL, time.Now()
	clearMustChangePassword(t, srv, u.ID)
	return srv, u
}

// webPairAndRegister mints a token exactly as Security → Devices does (session
// plus step-up) and redeems it at native/register through the router.
func webPairAndRegister(t *testing.T, srv *Server, userID, deviceID string) (*httptest.ResponseRecorder, pendingRegistration) {
	t.Helper()
	mint := deviceGrantCall(t, srv, userID, http.MethodPost, "/api/notifications/pairing", pairingStepUpBody(t, srv, userID))
	var pairing struct {
		SubscriberID string `json:"subscriberId"`
		Token        string `json:"pairingToken"`
	}
	if mint.Code != http.StatusOK || json.Unmarshal(mint.Body.Bytes(), &pairing) != nil || pairing.Token == "" {
		t.Fatalf("mint pairing: %d %s", mint.Code, mint.Body.String())
	}
	return registerToken(t, srv, pairing.SubscriberID, pairing.Token, deviceID)
}

func registerToken(t *testing.T, srv *Server, subscriberID, token, deviceID string) (*httptest.ResponseRecorder, pendingRegistration) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"subscriberId": subscriberID, "pairingToken": token, "deviceToken": "tok-" + deviceID,
		"deviceId": deviceID, "platform": "android", "deviceName": "Pixel 9", "osVersion": "Android 16",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/native/register", jsonBody(body))
	req.RemoteAddr = "203.0.113.7:4444"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	var out pendingRegistration
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func deviceCall(srv *Server, path, deviceID, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	setDeviceHeaders(req, deviceID, secret)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func pairingStatus(srv *Server, deviceID, secret string) *httptest.ResponseRecorder {
	return deviceCall(srv, "/api/notifications/native/pairing-status", deviceID, secret)
}

func decide(t *testing.T, srv *Server, userID, deviceID, decision string) *httptest.ResponseRecorder {
	t.Helper()
	return deviceGrantCall(t, srv, userID, http.MethodPost, "/api/notifications/native/pending/"+deviceID+"/"+decision, "")
}

func pendingIDs(t *testing.T, srv *Server, userID string) []string {
	t.Helper()
	rec := deviceGrantCall(t, srv, userID, http.MethodGet, "/api/notifications/native/pending", "")
	var out struct {
		Pending []struct {
			DeviceID string `json:"deviceId"`
		} `json:"pending"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list pending: %d %s", rec.Code, rec.Body.String())
	}
	ids := []string{}
	for _, p := range out.Pending {
		ids = append(ids, p.DeviceID)
	}
	return ids
}

func TestWebPairedDeviceIsPendingWithoutAccess(t *testing.T) {
	srv, u := newPendingServer(t)
	rec, reg := webPairAndRegister(t, srv, u.ID, "phone-1")
	if rec.Code != http.StatusAccepted || reg.Status != "pending" || reg.DeviceSecret == "" || len(reg.ApprovalCode) != 6 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if reg.StatusEndpoint != "https://mail.example/api/notifications/native/pairing-status" {
		t.Fatalf("statusEndpoint = %q", reg.StatusEndpoint)
	}
	store, _ := srv.userStore(u.ID)
	if devices := mustNativeDevices(t, store); len(devices) != 0 {
		t.Fatalf("pending device was persisted: %+v", devices)
	}
	if got := deviceCall(srv, "/api/notifications/native/pull", reg.DeviceID, reg.DeviceSecret); got.Code != http.StatusUnauthorized {
		t.Fatalf("pending device pull = %d, want 401", got.Code)
	}
	if got := deviceCall(srv, "/api/inbox?mailbox=INBOX", reg.DeviceID, reg.DeviceSecret); got.Code != http.StatusUnauthorized {
		t.Fatalf("pending device inbox = %d, want 401", got.Code)
	}
	if got := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); got.Code != http.StatusAccepted || !strings.Contains(got.Body.String(), `"pending"`) {
		t.Fatalf("status = %d %s", got.Code, got.Body.String())
	}
	if got := pairingStatus(srv, reg.DeviceID, "wrong"); got.Code != http.StatusGone {
		t.Fatalf("status with wrong secret = %d, want 410", got.Code)
	}

	list := deviceGrantCall(t, srv, u.ID, http.MethodGet, "/api/notifications/native/pending", "")
	for _, want := range []string{`"deviceName":"Pixel 9"`, `"osVersion":"Android 16"`, `"requestIp":"203.0.113.7"`, `"approvalCode":"` + reg.ApprovalCode + `"`} {
		if !strings.Contains(list.Body.String(), want) {
			t.Fatalf("pending list lacks %s: %s", want, list.Body.String())
		}
	}
	if strings.Contains(list.Body.String(), reg.DeviceSecret) || strings.Contains(list.Body.String(), "secretHash") {
		t.Fatal("pending list leaks the device secret")
	}
}

func TestApprovingPendingDeviceGrantsAccess(t *testing.T) {
	srv, u := newPendingServer(t)
	_, reg := webPairAndRegister(t, srv, u.ID, "phone-1")
	if rec := decide(t, srv, u.ID, reg.DeviceID, "approve"); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if got := deviceCall(srv, "/api/notifications/native/pull", reg.DeviceID, reg.DeviceSecret); got.Code != http.StatusOK {
		t.Fatalf("approved device pull = %d %s", got.Code, got.Body.String())
	}
	st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret)
	if st.Code != http.StatusOK || !strings.Contains(st.Body.String(), `"status":"approved"`) || !strings.Contains(st.Body.String(), `"deviceId":"phone-1"`) {
		t.Fatalf("status after approve = %d %s", st.Code, st.Body.String())
	}
	if ids := pendingIDs(t, srv, u.ID); len(ids) != 0 {
		t.Fatalf("approved device still listed as pending: %v", ids)
	}
	if rec := decide(t, srv, u.ID, reg.DeviceID, "approve"); rec.Code != http.StatusNotFound {
		t.Fatalf("second approve = %d, want 404", rec.Code)
	}
	// Once the pending entry is gone the status answer comes from the
	// paired-device list.
	srv.pairingMu.Lock()
	delete(srv.pendingDevices, reg.DeviceID)
	srv.pairingMu.Unlock()
	if st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); st.Code != http.StatusOK {
		t.Fatalf("status from device list = %d %s", st.Code, st.Body.String())
	}
}

func TestDenyingPendingDeviceDeletesIt(t *testing.T) {
	srv, u := newPendingServer(t)
	_, reg := webPairAndRegister(t, srv, u.ID, "phone-1")
	if rec := decide(t, srv, u.ID, reg.DeviceID, "deny"); rec.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}
	if st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); st.Code != http.StatusGone {
		t.Fatalf("status after deny = %d, want 410", st.Code)
	}
	if rec := decide(t, srv, u.ID, reg.DeviceID, "approve"); rec.Code != http.StatusNotFound {
		t.Fatalf("approve after deny = %d, want 404", rec.Code)
	}
	if ids := pendingIDs(t, srv, u.ID); len(ids) != 0 {
		t.Fatalf("denied device still pending: %v", ids)
	}
	store, _ := srv.userStore(u.ID)
	if devices := mustNativeDevices(t, store); len(devices) != 0 {
		t.Fatalf("denied device was persisted: %+v", devices)
	}
}

func TestPendingDeviceExpires(t *testing.T) {
	srv, u := newPendingServer(t)
	_, reg := webPairAndRegister(t, srv, u.ID, "phone-1")
	srv.pairingMu.Lock()
	p := srv.pendingDevices[reg.DeviceID]
	if p == nil || p.expiresAt.Sub(p.requestedAt) != pendingDeviceTTL || pendingDeviceTTL > 5*time.Minute {
		srv.pairingMu.Unlock()
		t.Fatalf("pending entry %+v, want a %v lifetime", p, pendingDeviceTTL)
	}
	p.expiresAt = time.Now().Add(-time.Second)
	srv.pairingMu.Unlock()

	if st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); st.Code != http.StatusGone {
		t.Fatalf("status after expiry = %d, want 410", st.Code)
	}
	if rec := decide(t, srv, u.ID, reg.DeviceID, "approve"); rec.Code != http.StatusNotFound {
		t.Fatalf("approve after expiry = %d, want 404", rec.Code)
	}
	srv.pairingMu.Lock()
	_, kept := srv.pendingDevices[reg.DeviceID]
	srv.pairingMu.Unlock()
	if kept {
		t.Fatal("expired pending device was not deleted")
	}
}

func TestOtherUserCannotDecidePendingDevice(t *testing.T) {
	srv, u := newPendingServer(t)
	other, err := srv.users.Create(context.Background(), "mallory", deviceGrantPassword, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	clearMustChangePassword(t, srv, other.ID)
	_, reg := webPairAndRegister(t, srv, u.ID, "phone-1")

	if ids := pendingIDs(t, srv, other.ID); len(ids) != 0 {
		t.Fatalf("another user sees the pending device: %v", ids)
	}
	for _, decision := range []string{"approve", "deny"} {
		if rec := decide(t, srv, other.ID, reg.DeviceID, decision); rec.Code != http.StatusNotFound {
			t.Fatalf("%s by another user = %d, want 404", decision, rec.Code)
		}
	}
	if st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); st.Code != http.StatusAccepted {
		t.Fatalf("device left pending = %d, want 202", st.Code)
	}
	// Without a session the routes refuse outright.
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/native/pending/"+reg.DeviceID+"/approve", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous approve = %d, want 401", rec.Code)
	}
}

// A client that predates approval only treats HTTP 200 with ok and synced as a
// paired device, and the secret it got does nothing until approval.
func TestOldClientGetsNoAccessWithoutApproval(t *testing.T) {
	srv, u := newPendingServer(t)
	rec, reg := webPairAndRegister(t, srv, u.ID, "old-phone")
	if rec.Code == http.StatusOK || reg.OK || reg.Synced {
		t.Fatalf("old client would read this as paired: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"approve this device`) {
		t.Fatalf("no usable error for an old client: %s", rec.Body.String())
	}
	for _, path := range []string{"/api/notifications/native/pull", "/api/contacts/sync", "/api/client/version"} {
		if got := deviceCall(srv, path, reg.DeviceID, reg.DeviceSecret); got.Code == http.StatusOK {
			t.Fatalf("pending credential reached %s", path)
		}
	}
}

func TestNoWayToSkipApproval(t *testing.T) {
	srv, u := newPendingServer(t)
	store, _ := srv.userStore(u.ID)
	sub, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatal(err)
	}

	// A token without the direct claim (every web-minted token, and every
	// token minted before this change) is held pending.
	plain, _, _ := srv.createPairingToken(sub, pairingPurposeNativeDevice, time.Minute)
	if rec, _ := registerToken(t, srv, sub, plain, "plain"); rec.Code != http.StatusAccepted {
		t.Fatalf("plain token = %d, want 202", rec.Code)
	}

	// The claim is signed: adding it to a web token breaks the signature.
	parts := strings.SplitN(plain, ".", 2)
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	claims["direct"] = true
	forged, _ := json.Marshal(claims)
	tampered := base64.RawURLEncoding.EncodeToString(forged) + "." + parts[1]
	if rec, _ := registerToken(t, srv, sub, tampered, "forged"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged direct claim = %d, want 401", rec.Code)
	}

	// Request fields cannot ask for it either.
	other, _, _ := srv.createPairingToken(sub, pairingPurposeNativeDevice, time.Minute)
	body, _ := json.Marshal(map[string]any{"subscriberId": sub, "pairingToken": other, "deviceToken": "tok-x", "deviceId": "fields", "direct": true, "status": "approved"})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/native/register", jsonBody(body))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request asking to skip = %d, want 202", rec.Code)
	}
	if devices := mustNativeDevices(t, store); len(devices) != 0 {
		t.Fatalf("a device skipped approval: %+v", devices)
	}

	// Only the direct-response flows mint the claim; the web pairing response
	// never does.
	mint := deviceGrantCall(t, srv, u.ID, http.MethodPost, "/api/notifications/pairing", pairingStepUpBody(t, srv, u.ID))
	var minted struct {
		Token string `json:"pairingToken"`
	}
	_ = json.Unmarshal(mint.Body.Bytes(), &minted)
	got, err := srv.decodeAndVerifyPairingToken(minted.Token, pairingPurposeNativeDevice, time.Now())
	if err != nil || got.Direct {
		t.Fatalf("web pairing token direct=%v err=%v", got.Direct, err)
	}
}

// Sign-on and review pairing hand the token straight back to the client that
// proved the credential, so their devices register at once.
func TestDirectTokenRegistersAtOnce(t *testing.T) {
	srv, u := newPendingServer(t)
	store, _ := srv.userStore(u.ID)
	sub, _ := store.GetOrCreateSubscriberID()
	token, _, err := srv.mintPairingToken(sub, pairingPurposeNativeDevice, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	rec, reg := registerToken(t, srv, sub, token, "direct")
	if rec.Code != http.StatusOK || !reg.OK || reg.Status != "approved" {
		t.Fatalf("direct register: %d %s", rec.Code, rec.Body.String())
	}
	if got := deviceCall(srv, "/api/notifications/native/pull", reg.DeviceID, reg.DeviceSecret); got.Code != http.StatusOK {
		t.Fatalf("direct device pull = %d", got.Code)
	}
}

func TestCredentialRevocationDropsPendingDevices(t *testing.T) {
	srv, u := newPendingServer(t)
	_, reg := webPairAndRegister(t, srv, u.ID, "phone-1")
	fresh, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.revokeAllUserCredentials(fresh); err != nil {
		t.Fatal(err)
	}
	if st := pairingStatus(srv, reg.DeviceID, reg.DeviceSecret); st.Code != http.StatusGone {
		t.Fatalf("status after revocation = %d, want 410", st.Code)
	}
}

func TestPendingDevicesPerUserAreCapped(t *testing.T) {
	srv, u := newPendingServer(t)
	for i := range maxPendingDevicesPerUser {
		if rec, _ := webPairAndRegister(t, srv, u.ID, "phone-"+string(rune('a'+i))); rec.Code != http.StatusAccepted {
			t.Fatalf("pending %d: %d", i, rec.Code)
		}
	}
	if rec, _ := webPairAndRegister(t, srv, u.ID, "phone-over"); rec.Code != http.StatusConflict {
		t.Fatalf("over the cap = %d, want 409", rec.Code)
	}
}
