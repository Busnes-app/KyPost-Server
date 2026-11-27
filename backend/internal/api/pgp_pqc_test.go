package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestPQCConversionHTTPAndReadiness(t *testing.T) {
	s := newTestServer(t)
	u := clientProtectedUser(t, s)
	const address = "ring@example.invalid"
	writeUnreachableSMTPIMAPConfig(t, s, u.ID, address)
	v4, err := pgpmailtest.GenerateIdentity("Ring", address)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := pgpmail.InspectPublicKey(v4.ArmoredPublicKey)
	u, err = s.users.SetPGPIdentityClientProtected(u.ID, old.Fingerprint, old.KeyID, old.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	u, err = s.users.SetDerivedAuth(context.Background(), u.ID, secret, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false)
	if err != nil {
		t.Fatal(err)
	}
	pq, err := pgpmailtest.GenerateV6PublicKey("Ring", address, true)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := pgpmail.InspectPQCKey(pq)
	body := map[string]any{"expectedRevision": u.PGPRevision, "expectedFingerprint": u.PGPFingerprint, "publicKeyV6": pq,
		"wrapped": `{"v":2,"ring":"password"}`, "recoveryEnvelope": `{"v":2,"ring":"recovery"}`, "authSecret": secret,
		"keyring": users.PGPKeyringState{Version: 2, MaterialGeneration: 1, PrimaryFingerprints: []string{u.PGPFingerprint, info.Fingerprint}, KeyFingerprints: append(old.KeyFingerprints, info.KeyFingerprints...)}}
	// Build one stable session: authRequestAs itself updates the account's
	// onboarding timestamp and must not run inside a no-mutation assertion.
	sessionRequest := httptest.NewRequest("POST", "/api/pgp/identity/pqc", nil)
	authRequestAs(s, sessionRequest, u.ID)
	u, err = s.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	post := func() *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/pgp/identity/pqc", bytes.NewReader(encoded))
		for _, cookie := range sessionRequest.Cookies() {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", sessionRequest.Header.Get("X-CSRF-Token"))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	// Wrong credentials, missing guards, malformed profile, foreign UID: no write.
	for _, key := range []string{"authSecret", "expectedRevision", "publicKeyV6"} {
		saved := body[key]
		delete(body, key)
		if w := post(); w.Code < 400 {
			t.Fatal(key, w.Code, w.Body.String())
		}
		body[key] = saved
	}
	foreign, _ := pgpmailtest.GenerateV6PublicKey("Foreign", "other@example.invalid", true)
	body["publicKeyV6"] = foreign
	if w := post(); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	body["publicKeyV6"] = pq
	unchanged, _ := s.users.Get(u.ID)
	if !reflect.DeepEqual(u, unchanged) {
		t.Fatal("rejected conversion mutated")
	}
	// A paired Linux device without v6 blocks storage despite successful conversion.
	device, _ := pairNativeDevice(t, s, u.ID, "linux")
	if w := post(); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	current, _ := s.users.Get(u.ID)
	if current.PGPFingerprint != u.PGPFingerprint || current.PGPKeyring.Version != 2 || current.PGPFingerprintV6 != info.Fingerprint {
		t.Fatal("lost v4 or PQ identity")
	}
	if s.pgpV6Ready(current) {
		t.Fatal("legacy Linux enabled v6 encryption")
	}
	store, _ := s.userStore(u.ID)
	caps := []string{"v4", "v6", "rfc9980"}
	if _, err := store.SetNativeDeviceEnrollmentKey(device, "KEY", "now", []int{6}, caps); err != nil {
		t.Fatal(err)
	}
	if s.pgpV6Ready(current) {
		t.Fatal("codecs enabled v6 before ring delivery")
	}
	delivery := state.DeviceEnrollment{Version: 6, Generation: 1, Fingerprint: u.PGPFingerprint}
	if err := store.RecordNativeDeviceDelivery(device, delivery); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConfirmNativeDeviceEnrollment(device, delivery, caps); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !s.pgpV6Ready(current) {
		t.Fatal("confirmed capable ring did not enable v6")
	}
	req := httptest.NewRequest("GET", "/api/pgp/bootstrap", nil)
	authRequestAs(s, req, u.ID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"v6Ready":true`) || !strings.Contains(w.Body.String(), `"publicKeyV6"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := post(); w.Code != http.StatusConflict {
		t.Fatal("stale revision accepted", w.Code)
	}
}
