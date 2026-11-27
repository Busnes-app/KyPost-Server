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
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
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

// pqAccount is a client-protected account; a non-empty pq adds that internal
// certificate. With no paired devices a committed certificate is ready.
func pqAccount(t *testing.T, s *Server, name, pq string) users.User {
	t.Helper()
	u := must1(s.users.Create(context.Background(), name, name+"-testpassword", users.RoleUser))
	v4 := must1(pgpmailtest.GenerateIdentity(name, name+"@example.invalid"))
	old := must1(pgpmail.InspectPublicKey(v4.ArmoredPublicKey))
	u = must1(s.users.SetPGPIdentityClientProtected(u.ID, old.Fingerprint, old.KeyID, old.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil))
	u = must1(s.users.SetDerivedAuth(context.Background(), u.ID, strings.Repeat("a", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false))
	if pq == "" {
		return u
	}
	info := must1(pgpmail.InspectPQCKey(pq))
	return must1(s.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{ExpectedRevision: u.PGPRevision, MaterialGeneration: 1,
		PrimaryFingerprints: []string{u.PGPFingerprint, info.Fingerprint}, KeyFingerprints: append(old.KeyFingerprints, info.KeyFingerprints...),
		PublicKey: u.PGPPublicKey, PublicKeyV6: pq, PasswordEnvelope: `{"v":2}`, RecoveryEnvelope: `{"v":2}`, Source: "generated"}))
}

func proveAlias(t *testing.T, s *Server, userID, address string) {
	t.Helper()
	store := must1(s.userSendAsStore(userID))
	alias := must1(store.Create(userID, address, ""))
	if err := store.MarkVerified(alias.ID); err != nil {
		t.Fatal(err)
	}
}

type recipientAnswer struct {
	Address, PublicKey, Capability string
	HasKey, Usable                 bool
}

// askRecipient posts one address to a recipients endpoint ("check" or "resolve").
func askRecipient(t *testing.T, s *Server, senderID, endpoint, address string, sign bool) recipientAnswer {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"addresses": []string{address}, "sign": sign})
	r := httptest.NewRequest("POST", "/api/pgp/recipients/"+endpoint, bytes.NewReader(body))
	authRequestAs(s, r, senderID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	var out struct{ Results []recipientAnswer }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Results) != 1 {
		t.Fatalf("%s: %d %s", endpoint, w.Code, w.Body.String())
	}
	return out.Results[0]
}

// An IMAP username is self-declared: claiming a colleague's address with it
// must never make another account's discovery return the claimant's key, and
// an address two accounts prove belongs to neither.
func TestInternalPQDiscoveryRequiresProvenOwnership(t *testing.T) {
	s := newTestServer(t)
	sender := pqAccount(t, s, "sender", testV6Key(t, "sender@example.invalid", true))
	claimant := pqAccount(t, s, "claimant", testV6Key(t, "claimant@example.invalid", true))
	writeUnreachableSMTPIMAPConfig(t, s, claimant.ID, "victim@example.invalid")
	if got := askRecipient(t, s, sender.ID, "check", "victim@example.invalid", false); got.HasKey || got.Capability == "pq" {
		t.Fatalf("unverified IMAP username won discovery: %+v", got)
	}
	const shared = "shared@example.invalid"
	proveAlias(t, s, claimant.ID, shared)
	if got := askRecipient(t, s, sender.ID, "check", shared, false); !got.HasKey || got.Capability != "pq" {
		t.Fatalf("domain-proven alias not discovered: %+v", got)
	}
	resolver := &keyResolver{store: must1(s.userContactsStore(sender.ID)), preferV6: true}
	if rk, found, err := s.localPQRecipient(sender, shared, resolver); err != nil || !found || rk.Armored != claimant.PGPPublicKeyV6 {
		t.Fatal("proven owner's certificate not returned", found, err)
	}
	rival := pqAccount(t, s, "rival", testV6Key(t, "rival@example.invalid", true))
	proveAlias(t, s, rival.ID, shared)
	if _, found, err := s.localPQRecipient(sender, shared, resolver); err != nil || found {
		t.Fatal("ambiguous ownership selected an account", found, err)
	}
	if got := askRecipient(t, s, sender.ID, "check", shared, false); got.HasKey || got.Capability == "pq" {
		t.Fatalf("ambiguous address discovered: %+v", got)
	}
}

// A signed delivery needs a signing certificate of the recipient key's
// version, so a v4-only signer keeps the v4 key; everyone else prefers v6.
func TestRecipientVersionFollowsSenderSigningCertificates(t *testing.T) {
	s := newTestServer(t)
	v4Only := pqAccount(t, s, "classic", "")
	dual := pqAccount(t, s, "dual", testV6Key(t, "dual@example.invalid", true))
	internal := pqAccount(t, s, "internal", testV6Key(t, "internal@example.invalid", true))
	proveAlias(t, s, internal.ID, "internal@example.invalid")
	const bob = "bob@example.invalid"
	k4, k6 := testV4Key(t, bob), testV6Key(t, bob, true)
	for _, u := range []users.User{v4Only, dual} {
		must1(must1(s.userContactsStore(u.ID)).Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: bob}},
			PGPKey: k4, PGPKeyFingerprint: fingerprintOf(t, k4), PGPKeyV6: k6, PGPKeyV6Fingerprint: fingerprintOf(t, k6)}))
	}
	for _, tc := range []struct {
		sender users.User
		sign   bool
		want   string
		pq     bool
	}{{v4Only, true, k4, false}, {v4Only, false, k6, true}, {dual, true, k6, true}, {dual, false, k6, true}} {
		if got := askRecipient(t, s, tc.sender.ID, "resolve", bob, tc.sign); !got.Usable || strings.TrimSpace(got.PublicKey) != strings.TrimSpace(tc.want) {
			t.Fatalf("%s sign=%v: got %+v", tc.sender.Username, tc.sign, got)
		}
		if got := askRecipient(t, s, tc.sender.ID, "check", "internal@example.invalid", tc.sign); (got.Capability == "pq") != tc.pq {
			t.Fatalf("%s sign=%v internal: %+v", tc.sender.Username, tc.sign, got)
		}
	}
}

// An installed v6 certificate that has expired stops being the target the
// moment it expires, whatever the device gate says; the v4 key carries on.
func TestExpiredV6FallsBackToV4(t *testing.T) {
	s := newTestServer(t)
	const address = "expiring@example.invalid"
	pq := must1(pgpmailtest.GenerateExpiringV6PublicKey("Expiring", address, 3*time.Second))
	recipient := pqAccount(t, s, "expiring", pq)
	proveAlias(t, s, recipient.ID, address)
	sender := pqAccount(t, s, "sender", testV6Key(t, "sender@example.invalid", true))
	must1(must1(s.userContactsStore(sender.ID)).Upsert(contacts.Contact{FormattedName: "Expiring", Emails: []contacts.ContactValue{{Value: address}},
		PGPKey: recipient.PGPPublicKey, PGPKeyFingerprint: recipient.PGPFingerprint}))
	if !s.pgpV6Ready(recipient) || askRecipient(t, s, sender.ID, "resolve", address, true).Capability != "pq" {
		t.Fatal("fresh v6 certificate not used")
	}
	for deadline := time.Now().Add(10 * time.Second); pgpmail.EncryptsNow(pq); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("certificate never expired")
		}
	}
	if s.pgpV6Ready(recipient) {
		t.Fatal("expired v6 certificate still ready")
	}
	if got := askRecipient(t, s, sender.ID, "resolve", address, true); !got.Usable || got.PublicKey != recipient.PGPPublicKey {
		t.Fatalf("expired v6 did not fall back to v4: %+v", got)
	}
}
