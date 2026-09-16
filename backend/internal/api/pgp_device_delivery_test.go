package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busness-app/kypost-server/backend/internal/state"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

const (
	deliveryDeviceKey = "BGsX0fLhLEJH+Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT+NC4v4af5uO5+tKfA+eFivOM1drMV7Oy7ZAaDe/UfU="
	deliveryAuth      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// sealedEnvelope is a well-formed sealing of the given version; the server
// never opens one, so zero bytes of the right lengths are all it checks.
func sealedEnvelope(version int) string { return sealedEnvelopeFilled(version, 0) }

// sealedEnvelopeFilled is sealedEnvelope with a distinguishable ciphertext.
func sealedEnvelopeFilled(version int, fill byte) string {
	epk := make([]byte, 65)
	epk[0] = 4
	ct := bytes.Repeat([]byte{fill}, 16)
	return `{"v":` + string(rune('0'+version)) + `,"alg":"ECDH-P256+HKDF-SHA256+A256GCM","epk":"` +
		base64.StdEncoding.EncodeToString(epk) + `","iv":"` + base64.StdEncoding.EncodeToString(make([]byte, 12)) +
		`","ct":"` + base64.StdEncoding.EncodeToString(ct) + `"}`
}

// convertedUser is a client-protected account with a complete keyring at
// generation 1 and derived auth, so the step-up takes deliveryAuth.
func convertedUser(t *testing.T, srv *Server) users.User {
	t.Helper()
	u := clientProtectedUser(t, srv)
	identity, err := pgpmail.GenerateIdentity("Ring", "ring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	u, err = srv.users.SetPGPIdentityClientProtected(u.ID, info.Fingerprint, info.KeyID, info.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil)
	if err != nil {
		t.Fatal(err)
	}
	salt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	u, err = srv.users.SetDerivedAuth(context.Background(), u.ID, deliveryAuth, salt, 600000, false)
	if err != nil {
		t.Fatal(err)
	}
	u, err = srv.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{
		ExpectedRevision: u.PGPRevision, MaterialGeneration: 1, PrimaryFingerprints: []string{info.Fingerprint}, KeyFingerprints: info.KeyFingerprints,
		PublicKey: info.ArmoredPublicKey, PasswordEnvelope: `{"v":2,"password":"ring"}`, RecoveryEnvelope: `{"v":2,"recovery":"ring"}`, Source: "generated",
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// publishDevice pairs a device that published an enrollment key with the
// given version claims, and returns its credential stamp.
func publishDevice(t *testing.T, srv *Server, userID, deviceID string, versions []int) (func(*http.Request), state.NativeDevice) {
	t.Helper()
	id, secret := pairNativeDevice(t, srv, userID, deviceID)
	store, err := srv.userStore(userID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.SetNativeDeviceEnrollmentKey(id, deliveryDeviceKey, "2026-09-16T00:00:00Z", versions)
	if err != nil {
		t.Fatal(err)
	}
	return func(r *http.Request) { setDeviceHeaders(r, id, secret) }, d
}

func sessionJSON(t *testing.T, srv *Server, userID, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func deviceJSON(t *testing.T, srv *Server, auth func(*http.Request), method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	auth(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestDeviceDeliveryIsBoundToKeyVersionAndGeneration(t *testing.T) {
	srv := newTestServer(t)
	u := convertedUser(t, srv)
	deviceAuth, _ := publishDevice(t, srv, u.ID, "phone", []int{2, 3})
	slot := "/api/pgp/identity/envelope/device:phone"
	base := func() map[string]any {
		return map[string]any{"envelope": sealedEnvelope(3), "expectedRevision": u.PGPRevision, "expectedFingerprint": u.PGPFingerprint,
			"enrollmentPublicKey": deliveryDeviceKey, "materialGeneration": 1, "authSecret": deliveryAuth}
	}
	refused := []struct {
		name   string
		mutate func(m map[string]any)
		status int
		marker string
	}{
		{"no enrollment key", func(m map[string]any) { delete(m, "enrollmentPublicKey") }, 400, "enrollmentPublicKey"},
		{"replaced enrollment key", func(m map[string]any) { m["enrollmentPublicKey"] = "BOther" }, 409, `"enrollmentChanged":true`},
		{"legacy v2 on a converted account", func(m map[string]any) { m["envelope"] = sealedEnvelope(2) }, 409, `"deviceEnvelopeVersion":true`},
		{"malformed envelope", func(m map[string]any) { m["envelope"] = `{"v":3}` }, 400, "well-formed"},
		{"stale generation", func(m map[string]any) { m["materialGeneration"] = 0 }, 409, `"pgpStateChanged":true`},
		{"stale revision", func(m map[string]any) { m["expectedRevision"] = u.PGPRevision + 5 }, 409, "PGP state changed"},
		{"no fingerprint", func(m map[string]any) { delete(m, "expectedFingerprint") }, 400, "fingerprint"},
		{"unknown device", func(m map[string]any) {}, 404, "no such paired device"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			body := base()
			tc.mutate(body)
			path := slot
			if tc.name == "unknown device" {
				path = "/api/pgp/identity/envelope/device:ghost"
			}
			rec := sessionJSON(t, srv, u.ID, http.MethodPut, path, body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.marker) {
				t.Fatalf("status = %d, want %d with %q: %s", rec.Code, tc.status, tc.marker, rec.Body.String())
			}
		})
	}
	after, _ := srv.users.Get(u.ID)
	for _, e := range after.WrappedEnvelopes() {
		if strings.HasPrefix(e.Slot, users.EnvelopeSlotDevicePrefix) {
			t.Fatalf("a refused delivery stored a slot: %+v", e)
		}
	}

	// A device that never claimed v3 cannot be sent one.
	v2Auth, _ := publishDevice(t, srv, u.ID, "oldphone", nil)
	body := base()
	rec := sessionJSON(t, srv, u.ID, http.MethodPut, "/api/pgp/identity/envelope/device:oldphone", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"envelopeVersions":[2]`) {
		t.Fatalf("v3 to a v2-only device: %d %s", rec.Code, rec.Body.String())
	}
	if rec := deviceJSON(t, srv, v2Auth, http.MethodGet, "/api/pgp/device/envelope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the v2-only device found an envelope: %d", rec.Code)
	}

	// The real thing: delivered, and the device reads it back with the
	// snapshot it was prepared from.
	rec = sessionJSON(t, srv, u.ID, http.MethodPut, slot, base())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":3`) {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
	}
	rec = deviceJSON(t, srv, deviceAuth, http.MethodGet, "/api/pgp/device/envelope", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("device fetch: %d %s", rec.Code, rec.Body.String())
	}
	var fetched struct {
		Envelope           string                 `json:"envelope"`
		Version            int                    `json:"version"`
		Fingerprint        string                 `json:"fingerprint"`
		MaterialGeneration uint64                 `json:"materialGeneration"`
		Revision           uint64                 `json:"pgpRevision"`
		Keyring            *users.PGPKeyringState `json:"keyring"`
		PublicKey          string                 `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Envelope != sealedEnvelope(3) || fetched.Version != 3 || fetched.Fingerprint != u.PGPFingerprint || fetched.MaterialGeneration != 1 ||
		fetched.Revision != after.PGPRevision || fetched.Keyring == nil || fetched.Keyring.MaterialGeneration != 1 || fetched.PublicKey == "" {
		t.Fatalf("fetched = %+v", fetched)
	}
}

func TestDeviceAcknowledgementIsGenerationAware(t *testing.T) {
	srv := newTestServer(t)
	u := convertedUser(t, srv)
	deviceAuth, _ := publishDevice(t, srv, u.ID, "phone", []int{2, 3})
	ack := "/api/pgp/device/enrollment-state"
	device := func() state.NativeDevice { return deviceByID(t, srv, u.ID, "phone") }

	// The legacy boolean is not proof of v3 enrollment.
	rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"generationRequired":true`) {
		t.Fatalf("bare boolean on a converted account: %d %s", rec.Code, rec.Body.String())
	}
	if device().EncryptionEnrolled {
		t.Fatal("a refused acknowledgement marked the device enrolled")
	}
	stale := []struct {
		name string
		body map[string]any
	}{
		{"wrong generation", map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 2, "fingerprint": u.PGPFingerprint}},
		{"wrong fingerprint", map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 1, "fingerprint": "0000"}},
		{"version not advertised", map[string]any{"encryptionEnrolled": true, "envelopeVersion": 4, "materialGeneration": 1, "fingerprint": u.PGPFingerprint}},
	}
	for _, tc := range stale {
		rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, tc.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"materialGeneration":1`) {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if device().EncryptionEnrolled {
			t.Fatalf("%s marked the device enrolled", tc.name)
		}
	}
	if rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("partial metadata: %d", rec.Code)
	}
	// A converted account only ever delivers v3; an advertised v2 is not an
	// enrollment this account could have produced.
	if rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true, "envelopeVersion": 2, "materialGeneration": 1, "fingerprint": u.PGPFingerprint}); rec.Code != http.StatusConflict {
		t.Fatalf("v2 acknowledgement on a converted account: %d %s", rec.Code, rec.Body.String())
	}
	if device().EncryptionEnrolled {
		t.Fatal("an impossible version marked the device enrolled")
	}
	// While the delivery is live, the acknowledgement must be of that delivery
	// to the key the device still publishes.
	if rec := sessionJSON(t, srv, u.ID, http.MethodPut, "/api/pgp/identity/envelope/device:phone", map[string]any{"envelope": sealedEnvelope(3), "expectedRevision": u.PGPRevision, "expectedFingerprint": u.PGPFingerprint, "enrollmentPublicKey": deliveryDeviceKey, "materialGeneration": 1, "authSecret": deliveryAuth}); rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
	}
	store, _ := srv.userStore(u.ID)
	if _, err := store.SetNativeDeviceEnrollmentKey("phone", "BReplaced", "2026-09-16T01:00:00Z", []int{2, 3}); err != nil {
		t.Fatal(err)
	}
	if rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 1, "fingerprint": u.PGPFingerprint}); rec.Code != http.StatusConflict {
		t.Fatalf("acknowledgement after republishing the key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.SetNativeDeviceEnrollmentKey("phone", deliveryDeviceKey, "2026-09-16T02:00:00Z", []int{2, 3}); err != nil {
		t.Fatal(err)
	}

	rec = deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 1, "fingerprint": strings.ToLower(u.PGPFingerprint)})
	if rec.Code != http.StatusOK {
		t.Fatalf("acknowledgement: %d %s", rec.Code, rec.Body.String())
	}
	if d := device(); !d.EncryptionEnrolled || d.EnrolledVersion != 3 || d.EnrolledGeneration != 1 || d.EnrolledFingerprint != u.PGPFingerprint {
		t.Fatalf("after acknowledgement: %+v", d)
	}

	// The owner sees it on the device inventory.
	req := httptest.NewRequest(http.MethodGet, "/api/notifications/native/devices", nil)
	authRequestAs(srv, req, u.ID)
	list := httptest.NewRecorder()
	srv.routes().ServeHTTP(list, req)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"enrolledGeneration":1`) {
		t.Fatalf("device listing: %d %s", list.Code, list.Body.String())
	}

	// Saying no forgets it.
	if rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": false}); rec.Code != http.StatusOK {
		t.Fatalf("un-enroll: %d", rec.Code)
	}
	if d := device(); d.EncryptionEnrolled || d.EnrolledGeneration != 0 {
		t.Fatalf("after un-enroll: %+v", d)
	}

	// A legacy account keeps the boolean semantics.
	legacy := newTestServer(t)
	lu := clientProtectedUser(t, legacy)
	legacyAuth, _ := publishDevice(t, legacy, lu.ID, "phone", nil)
	if rec := deviceJSON(t, legacy, legacyAuth, http.MethodPost, ack, map[string]any{"encryptionEnrolled": true}); rec.Code != http.StatusOK {
		t.Fatalf("legacy boolean: %d %s", rec.Code, rec.Body.String())
	}
	if d := deviceByID(t, legacy, lu.ID, "phone"); !d.EncryptionEnrolled || d.EnrolledGeneration != 0 {
		t.Fatalf("legacy after boolean: %+v", d)
	}
}

func TestClientSealedSendRefusesStaleMaterial(t *testing.T) {
	srv := newTestServer(t)
	u := convertedUser(t, srv)
	deviceAuth, _ := publishDevice(t, srv, u.ID, "phone", []int{2, 3})
	delivery := map[string]any{"recipients": []string{"bob@example.com"}, "ciphertext": wellFormedDelivery}
	send := func(auth func(*http.Request), body map[string]any) *httptest.ResponseRecorder {
		body["deliveries"] = []any{delivery}
		body["from"] = "ring@example.invalid"
		body["to"] = []string{"bob@example.com"}
		body["sentCopy"], body["sentCopyEncrypted"] = wellFormedDelivery, true
		return deviceJSON(t, srv, auth, http.MethodPost, "/api/mail/send-pgp", body)
	}
	sessionSend := func(body map[string]any) *httptest.ResponseRecorder {
		return send(func(r *http.Request) { authRequestAs(srv, r, u.ID) }, body)
	}

	if rec := sessionSend(map[string]any{}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"pgpStateChanged":true`) {
		t.Fatalf("no generation from a session: %d %s", rec.Code, rec.Body.String())
	}
	if rec := sessionSend(map[string]any{"materialGeneration": 2}); rec.Code != http.StatusConflict {
		t.Fatalf("stale generation from a session: %d %s", rec.Code, rec.Body.String())
	}
	// The gate passes for a session at the current generation; what follows
	// (no mail account configured) is a different refusal.
	if rec := sessionSend(map[string]any{"materialGeneration": 1}); rec.Code == http.StatusConflict {
		t.Fatalf("current generation from a session refused: %s", rec.Body.String())
	}

	// A device at the current generation must also be enrolled there.
	if rec := send(deviceAuth, map[string]any{"materialGeneration": 1}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"reenrollmentRequired":true`) {
		t.Fatalf("unenrolled device: %d %s", rec.Code, rec.Body.String())
	}
	if rec := deviceJSON(t, srv, deviceAuth, http.MethodPost, "/api/pgp/device/enrollment-state", map[string]any{"encryptionEnrolled": true, "envelopeVersion": 3, "materialGeneration": 1, "fingerprint": u.PGPFingerprint}); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(deviceAuth, map[string]any{"materialGeneration": 1}); rec.Code == http.StatusConflict {
		t.Fatalf("enrolled device at the current generation refused: %s", rec.Body.String())
	}

	// The owner removes the device's sealing: the device cannot send until it
	// enrolls again, whatever it restates on registration.
	if rec := sessionJSON(t, srv, u.ID, http.MethodPut, "/api/pgp/identity/envelope/device:phone", map[string]any{"envelope": sealedEnvelope(3), "expectedRevision": u.PGPRevision, "expectedFingerprint": u.PGPFingerprint, "enrollmentPublicKey": deliveryDeviceKey, "materialGeneration": 1, "authSecret": deliveryAuth}); rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body.String())
	}
	if rec := sessionJSON(t, srv, u.ID, http.MethodDelete, "/api/pgp/identity/envelope/device:phone", map[string]any{"expectedRevision": u.PGPRevision, "authSecret": deliveryAuth}); rec.Code != http.StatusOK {
		t.Fatalf("delete slot: %d %s", rec.Code, rec.Body.String())
	}
	if d := deviceByID(t, srv, u.ID, "phone"); d.EncryptionEnrolled || d.EnrolledGeneration != 0 {
		t.Fatalf("deleting the slot left the enrollment record: %+v", d)
	}
	if rec := send(deviceAuth, map[string]any{"materialGeneration": 1}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"reenrollmentRequired":true`) {
		t.Fatalf("device sends after its slot was removed: %d %s", rec.Code, rec.Body.String())
	}
	store, _ := srv.userStore(u.ID)
	if err := store.SetNativeDeviceEncryptionEnrolled("phone", true); err != nil {
		t.Fatal(err)
	}
	if rec := send(deviceAuth, map[string]any{"materialGeneration": 1}); rec.Code != http.StatusConflict {
		t.Fatalf("a bare restatement re-opened the send gate: %d", rec.Code)
	}

	// A legacy account is not gated; older clients send nothing.
	legacy := newTestServer(t)
	lu := clientProtectedUser(t, legacy)
	legacyAuth, _ := publishDevice(t, legacy, lu.ID, "phone", nil)
	body := map[string]any{"deliveries": []any{delivery}, "from": "alice@example.com", "to": []string{"bob@example.com"},
		"sentCopy": wellFormedDelivery, "sentCopyEncrypted": true}
	if rec := deviceJSON(t, legacy, legacyAuth, http.MethodPost, "/api/mail/send-pgp", body); rec.Code == http.StatusConflict {
		t.Fatalf("legacy device send gated: %s", rec.Body.String())
	}
}
