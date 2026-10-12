package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// pairLinuxDevice pairs a Linux client the way its registration lands in the
// store: platform linux, transport unifiedpush, an endpoint URL as the token,
// and WebPush keys only when withKeys.
func pairLinuxDevice(t *testing.T, srv *Server, userID, deviceID string, withKeys bool) (secret string) {
	t.Helper()
	store, err := srv.userStore(userID)
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	raw, err := randomToken(24)
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	hash, err := users.HashPassword(context.Background(), raw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	d := state.NativeDevice{
		DeviceID: deviceID, Platform: "linux", Transport: "unifiedpush",
		PushToken: "https://ntfy.example/" + deviceID, UserID: userID, MFAApprover: true, SecretHash: hash,
	}
	if withKeys {
		ua, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		d.P256DH = base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
		d.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	}
	if err := store.UpsertNativeDevice(d); err != nil {
		t.Fatalf("UpsertNativeDevice: %v", err)
	}
	srv.userMu.Lock()
	srv.deviceIndex[deviceID] = userID
	srv.userMu.Unlock()
	return raw
}

// A Linux client that registered WebPush keys can enable push approval, is
// addressed by the challenge with the documented payload, and can approve with
// the number match. A keyless Linux device on the same account is never sent
// the challenge. Pull mode keeps the test off the network; the UnifiedPush
// encryption itself is TestUnifiedPushSenderNoPlaintextFallback.
func TestPushMFAReachesEncryptedUnifiedPushDevice(t *testing.T) {
	srv := newTestServer(t)
	const username, password = "linus", "pw-linus-testpassword"
	u, err := srv.users.Create(context.Background(), username, password, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	clearMustChangePassword(t, srv, u.ID)
	enrollTOTP(t, srv, u.ID)
	keyed := pairLinuxDevice(t, srv, u.ID, "linux-keyed", true)
	pairLinuxDevice(t, srv, u.ID, "linux-keyless", false)
	store, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatalf("userStore: %v", err)
	}
	if err := store.SetNativeDeliveryMode(state.DeliveryModePull); err != nil {
		t.Fatalf("SetNativeDeliveryMode: %v", err)
	}
	enablePush(t, srv, u.ID)

	challengeID, methods := loginChallenge(t, srv, username, password)
	if !methodsContain(methods, "push") {
		t.Fatalf("methods = %v, want push offered", methods)
	}
	ch, ok := srv.mfaChallenges.Get(challengeID)
	if !ok {
		t.Fatal("challenge missing")
	}
	// Synchronous, so the assertion does not race the login's own goroutine.
	srv.dispatchPushChallenge(u.ID, challengeID, loginContext{ipAddress: "203.0.113.9", userAgent: "UA"}, ch.CreatedAt, ch.MatchDigits, ch.DecoyDigits)

	challengesFor := func(deviceID string) []map[string]string {
		got, _, err := store.PullNotificationsAfterStrict(deviceID, 0)
		if err != nil {
			t.Fatalf("PullNotificationsAfterStrict: %v", err)
		}
		var out []map[string]string
		for _, n := range got {
			if n.Data["type"] == "mfa_challenge" && n.Data["challengeId"] == challengeID {
				out = append(out, n.Data)
			}
		}
		return out
	}
	if got := challengesFor("linux-keyless"); len(got) != 0 {
		t.Fatalf("keyless UnifiedPush device was sent the challenge: %v", got)
	}
	got := challengesFor("linux-keyed")
	if len(got) == 0 {
		t.Fatal("encrypted UnifiedPush device was not sent the challenge")
	}
	data := got[len(got)-1]
	if data["matchDigits"] != ch.MatchDigits || data["issuedAt"] == "" || len(data["decoyDigits"]) != 5 {
		t.Fatalf("payload = %v, want matchDigits %q, issuedAt and two decoys", data, ch.MatchDigits)
	}
	// ContentPreview is off by default, so sign-in context stays off the wire.
	if _, leaked := data["ipAddress"]; leaked {
		t.Fatalf("ipAddress sent without ContentPreview: %v", data)
	}

	if rec := respondPush(srv, challengeID, "linux-keyed", keyed, true); rec.Code != http.StatusOK {
		t.Fatalf("respond: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if pollPush(srv, challengeID) != "approved" {
		t.Fatal("challenge not approved")
	}
}

// A keyless Linux device alone cannot turn push approval on, and the refusal
// tells the user what to fix.
func TestPushEnableRefusesKeylessUnifiedPushOnly(t *testing.T) {
	srv := newTestServer(t)
	u, err := srv.users.Create(context.Background(), "lena", "pw-lena-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	clearMustChangePassword(t, srv, u.ID)
	enrollTOTP(t, srv, u.ID)
	pairLinuxDevice(t, srv, u.ID, "linux-keyless", false)
	rec := doJSONAuth(srv, srv.withAuth(srv.handleMFAPushEnabled), http.MethodPut,
		"/api/mfa/push/enabled", map[string]bool{"enabled": true}, u.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}
