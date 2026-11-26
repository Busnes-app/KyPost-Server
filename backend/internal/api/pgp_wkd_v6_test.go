package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/ProtonMail/gopenpgp/v3/profile"
)

func testV4Key(t *testing.T, email string) string {
	t.Helper()
	id, err := pgpmailtest.GenerateIdentity("Bob", email)
	if err != nil {
		t.Fatal(err)
	}
	return id.ArmoredPublicKey
}

func testV6Key(t *testing.T, email string, pqc bool) string {
	t.Helper()
	k, err := pgpmailtest.GenerateV6PublicKey("Bob", email, pqc)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// testV4KeyWithMLKEM is a v4 key with an added ML-KEM-768+X25519 (ID 35)
// encryption subkey, which RFC 9980 §3.5 allows on a v4 certificate.
func testV4KeyWithMLKEM(t *testing.T, email string) string {
	t.Helper()
	key, err := crypto.PGP().KeyGeneration().AddUserId("Bob", email).New().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := key.GetEntity().AddEncryptionSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoMlkem768X25519}); err != nil {
		t.Fatal(err)
	}
	pub, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func binaryKey(t *testing.T, armored string) []byte {
	t.Helper()
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		t.Fatal(err)
	}
	b, err := key.GetPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fingerprintOf(t *testing.T, armored string) string {
	t.Helper()
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		t.Fatal(err)
	}
	return key.GetFingerprint()
}

func serveWKD(t *testing.T, body []byte) {
	t.Helper()
	allowLoopbackOutboundForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hu/") {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	wkdBaseURLOverride = srv.URL
	t.Cleanup(func() { wkdBaseURLOverride = "" })
}

// A WKD answer with several certificates is read whole and ordered by policy:
// v6 PQC > v4 with an ID 35 subkey > v4. A certificate that does not carry
// the address is dropped, whatever its class.
func TestWKDReadsEveryCertificateAndPicksV6PQC(t *testing.T) {
	v4 := testV4Key(t, "bob@example.com")
	v4pq := testV4KeyWithMLKEM(t, "bob@example.com")
	v6pq := testV6Key(t, "bob@example.com", true)
	foreign := testV6Key(t, "mallory@example.com", true)
	serveWKD(t, bytes.Join([][]byte{binaryKey(t, v4), binaryKey(t, foreign), binaryKey(t, v4pq), binaryKey(t, v6pq)}, nil))

	certs, err := fetchWKDKeys(context.Background(), "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range certs {
		got = append(got, c.Fingerprint)
	}
	want := []string{fingerprintOf(t, v6pq), fingerprintOf(t, v4pq), fingerprintOf(t, v4)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if c := certs[0].Class; !c.V6 || !c.PQ || certs[1].Class.V6 || !certs[1].Class.PQ {
		t.Fatalf("classes = %+v %+v", certs[0].Class, certs[1].Class)
	}
}

// Through the resolver: a recipient publishing v4 and v6 PQC reports "pq",
// pins both slots, and is still encrypted to with the v4 key in P0.
func TestResolveWKDV4AndV6PinsBothAndReportsPQ(t *testing.T) {
	v4, v6 := testV4Key(t, "bob@example.com"), testV6Key(t, "bob@example.com", true)
	serveWKD(t, append(binaryKey(t, v6), binaryKey(t, v4)...))
	store := must1(contacts.New(t.TempDir()))
	kr := &keyResolver{store: store, settings: pgpdiscovery.Settings{StoreDiscoveredKeys: true}, discover: true}

	rk := kr.resolve(context.Background(), "bob@example.com")
	if rk.Tier != tierWKD || !rk.Usable || rk.Capability != "pq" || rk.Fingerprint != fingerprintOf(t, v4) {
		t.Fatalf("resolve = tier %s usable %v capability %q fp %s", rk.Tier, rk.Usable, rk.Capability, rk.Fingerprint)
	}
	c, ok := must2(findContact(store, "bob@example.com"))
	if !ok || c.PGPKeyFingerprint != fingerprintOf(t, v4) || c.PGPKeyV6Fingerprint != fingerprintOf(t, v6) || c.PGPKeyV6Source != contacts.PGPSourceWKD {
		t.Fatalf("pinned contact = v4 %s v6 %s (%s)", c.PGPKeyFingerprint, c.PGPKeyV6Fingerprint, c.PGPKeyV6Source)
	}
	// The pinned contact now answers from step 1 with the same capability.
	if again := kr.resolve(context.Background(), "bob@example.com"); again.Tier != tierContactVerified || again.Capability != "pq" {
		t.Fatalf("second resolve = %s %q", again.Tier, again.Capability)
	}
}

// pickWKD against a pinned v4 key: a v6 key added beside the still-served v4
// key is not a change; a different v4 key is; a v6 key in place of the v4 key
// is.
func TestPickWKDKeepsV4PinAcrossV6Addition(t *testing.T) {
	k4, k6 := testV4Key(t, "bob@example.com"), testV6Key(t, "bob@example.com", true)
	c := contacts.Contact{PGPKey: k4, PGPKeyFingerprint: fingerprintOf(t, k4)}
	cert := func(armored string) wkdCert {
		key := must1(crypto.NewKeyFromArmored(armored))
		return wkdCert{Armored: armored, Fingerprint: key.GetFingerprint(), Class: classOfEntity(key.GetEntity())}
	}

	v4, v6, use, changed := pickWKD(c, []wkdCert{cert(k6), cert(k4)})
	if changed || v4 == nil || v6 == nil || use != v4 || v4.Fingerprint != c.PGPKeyFingerprint {
		t.Fatalf("v6 addition: changed=%v v4=%v v6=%v", changed, v4 != nil, v6 != nil)
	}
	// Each slot takes only its own version, whatever the answer's order.
	if v4, v6, _, _ := pickWKD(c, []wkdCert{cert(k4), cert(k6)}); v4 == nil || v6 == nil || v6.Fingerprint != fingerprintOf(t, k6) || v4.Fingerprint != c.PGPKeyFingerprint {
		t.Fatal("a slot took a certificate of the other version")
	}
	if _, _, _, changed := pickWKD(c, []wkdCert{cert(k6), cert(testV4Key(t, "bob@example.com"))}); !changed {
		t.Fatal("a different v4 key was not a key change")
	}
	if _, _, _, changed := pickWKD(c, []wkdCert{cert(k6)}); !changed {
		t.Fatal("a v6 key replacing the pinned v4 key was not a key change")
	}
	// A pinned v6 slot must match too.
	c.PGPKeyV6, c.PGPKeyV6Fingerprint = k6, fingerprintOf(t, k6)
	if _, _, _, changed := pickWKD(c, []wkdCert{cert(testV6Key(t, "bob@example.com", false)), cert(k4)}); !changed {
		t.Fatal("a different v6 key was not a key change")
	}
}

// With only a v6 key pinned, a v4 key newly served beside it is pinned into
// its slot but is not the encryption target: the pinned v6 key is.
func TestPickWKDEncryptsToThePinnedKey(t *testing.T) {
	k4, k6 := testV4Key(t, "bob@example.com"), testV6Key(t, "bob@example.com", false)
	c := contacts.Contact{PGPKey: k6, PGPKeyFingerprint: fingerprintOf(t, k6)}
	cert := func(armored string) wkdCert {
		key := must1(crypto.NewKeyFromArmored(armored))
		return wkdCert{Armored: armored, Fingerprint: key.GetFingerprint(), Class: classOfEntity(key.GetEntity())}
	}
	v4, _, use, changed := pickWKD(c, []wkdCert{cert(k4), cert(k6)})
	if changed || v4 == nil || use == nil || use.Fingerprint != c.PGPKeyFingerprint {
		t.Fatalf("changed=%v use=%v, want the pinned v6 key", changed, use)
	}
}

// A WKD lookup of a SECONDARY address cannot add a v6 key beside the contact's
// key: the key would then sign for the primary address too. The primary
// address (or a single-address contact) may.
func TestWKDPinAddsV6OnlyForTheWholeContactsAddress(t *testing.T) {
	k4 := testV4Key(t, "bob@example.com")
	k6 := testV6Key(t, "bob@other.example", true)
	cert := func(armored string) *wkdCert {
		key := must1(crypto.NewKeyFromArmored(armored))
		return &wkdCert{Armored: armored, Fingerprint: key.GetFingerprint(), Class: classOfEntity(key.GetEntity())}
	}
	store := must1(contacts.New(t.TempDir()))
	must1(store.Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}, {Value: "bob@other.example"}}, PGPKey: k4}))
	kr := &keyResolver{store: store}

	kr.pin("bob@other.example", cert(k4), cert(k6), contacts.PGPSourceWKD)
	c, _ := must2(findContact(store, "bob@example.com"))
	if c.PGPKeyV6 != "" {
		t.Fatal("a secondary-address WKD lookup added a v6 key to the whole contact")
	}
	for _, k := range must1(boundSignerKeysForSender(store, "bob@example.com")) {
		if k.PublicKey != "" && k.PublicKey != c.PGPKey {
			t.Fatal("the primary address gained a signer key from the secondary address's WKD")
		}
	}
	kr.pin("bob@example.com", cert(k4), cert(testV6Key(t, "bob@example.com", true)), contacts.PGPSourceWKD)
	if c, _ := must2(findContact(store, "bob@example.com")); c.PGPKeyV6 == "" {
		t.Fatal("the primary address's WKD lookup did not add its v6 key")
	}
}

// capability counts only keys that can be encrypted to now: a usable v4 key
// beside an expired v6 PQC key is "v4".
func TestCapabilityIgnoresUnusableKeys(t *testing.T) {
	k4 := testV4Key(t, "bob@example.com")
	exp, err := crypto.PGPWithProfile(profile.PQC()).KeyGeneration().AddUserId("Bob", "bob@example.com").
		GenerationTime(time.Now().Add(-48 * time.Hour).Unix()).Lifetime(3600).New().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k6 := must1(exp.GetArmoredPublicKey())
	c := contacts.Contact{PGPKey: k4, PGPKeyFingerprint: fingerprintOf(t, k4), PGPKeyV6: k6, PGPKeyV6Fingerprint: fingerprintOf(t, k6)}
	if got := contactCapability(c); got != "v4" {
		t.Fatalf("capability = %q, want v4", got)
	}
}
