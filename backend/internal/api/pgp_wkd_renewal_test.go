package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// expiredV6AndRenewal returns an expired v6 certificate carrying every email
// as a User ID and its renewal: the same key with a fresh direct-key signature
// and no expiry, so the same fingerprint.
func expiredV6AndRenewal(t *testing.T, emails ...string) (expired, renewed string) {
	t.Helper()
	past := time.Now().Add(-48 * time.Hour)
	cfg := &packet.Config{V6Keys: true, Algorithm: packet.PubKeyAlgoEd25519, KeyLifetimeSecs: 3600, Time: func() time.Time { return past }}
	e, err := openpgp.NewEntity("Bob", "", emails[0], cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range emails[1:] {
		if err := e.AddUserId("Bob", "", addr, cfg); err != nil {
			t.Fatal(err)
		}
	}
	armor := func() string {
		var b bytes.Buffer
		if err := e.Serialize(&b); err != nil {
			t.Fatal(err)
		}
		return must1(must1(crypto.NewKey(b.Bytes())).GetArmoredPublicKey())
	}
	expired = armor()
	now := &packet.Config{V6Keys: true}
	sig := *e.DirectSignatures[len(e.DirectSignatures)-1].Packet
	sig.CreationTime, sig.KeyLifetimeSecs, sig.SigLifetimeSecs = time.Now(), nil, nil
	if err := sig.SignDirectKeyBinding(&e.PrivateKey.PublicKey, e.PrivateKey, now); err != nil {
		t.Fatal(err)
	}
	e.DirectSignatures = append(e.DirectSignatures, packet.NewVerifiableSig(&sig))
	renewed = armor()
	if st := must1(pgpmail.CheckKeyStatus(expired)); st.Usable() {
		t.Fatal("expired fixture is usable")
	}
	if st := must1(pgpmail.CheckKeyStatus(renewed)); !st.Usable() {
		t.Fatal("renewed fixture is not usable")
	}
	if fingerprintOf(t, expired) != fingerprintOf(t, renewed) {
		t.Fatal("renewal changed the fingerprint")
	}
	return expired, renewed
}

// A WKD lookup of a contact's SECONDARY address may renew the pinned v6 key
// but must not install the new v4 key served beside it: that key would sign
// for, and be encrypted to as, the primary address.
func TestWKDSecondaryAddressRenewsButAddsNoV4(t *testing.T) {
	expired, renewed := expiredV6AndRenewal(t, "bob@example.com", "bob@other.example")
	pin6 := fingerprintOf(t, expired)
	attacker := testV4Key(t, "bob@other.example")
	serveWKD(t, append(binaryKey(t, renewed), binaryKey(t, attacker)...))

	store := must1(contacts.New(t.TempDir()))
	must1(store.Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}, {Value: "bob@other.example"}}, PGPKey: expired, PGPKeySource: "qr", PGPKeyVerified: true}))
	kr := &keyResolver{store: store, settings: pgpdiscovery.Settings{StoreDiscoveredKeys: true}, discover: true}

	for i := range 3 {
		rk := kr.resolve(context.Background(), "bob@other.example")
		if !rk.Usable || rk.Fingerprint != pin6 {
			t.Fatalf("resolution %d via the secondary address = %s %s, want the pinned v6 key", i, rk.Tier, rk.Fingerprint)
		}
		if p := kr.resolve(context.Background(), "bob@example.com"); !p.Usable || p.Fingerprint != pin6 {
			t.Fatalf("resolution %d of the primary address = %s %s, want the pinned v6 key", i, p.Tier, p.Fingerprint)
		}
		keys := must1(boundSignerKeysForSender(store, "bob@example.com"))
		if len(keys) != 1 || keys[0].Conflict || fingerprintOf(t, keys[0].PublicKey) != pin6 || !keys[0].Verified {
			t.Fatalf("resolution %d: primary signer keys = %+v", i, keys)
		}
	}
	c, _ := must2(findContact(store, "bob@example.com"))
	if c.PGPKeyV6 != "" || c.PGPKeyFingerprint != pin6 || !c.PGPKeyVerified {
		t.Fatalf("contact = key %s pin %s v6 %v", fingerprintOf(t, c.PGPKey), c.PGPKeyFingerprint, c.PGPKeyV6 != "")
	}
}

// The same answer for the PRIMARY address adds the v4 key and moves the v6
// key, with its pin and confirmation, into the v6 slot.
func TestWKDPrimaryAddressAddsV4AndKeepsV6Pin(t *testing.T) {
	expired, renewed := expiredV6AndRenewal(t, "bob@example.com", "bob@other.example")
	k4 := testV4Key(t, "bob@example.com")
	serveWKD(t, append(binaryKey(t, renewed), binaryKey(t, k4)...))
	store := must1(contacts.New(t.TempDir()))
	must1(store.Upsert(contacts.Contact{FormattedName: "Bob", Emails: []contacts.ContactValue{{Value: "bob@example.com"}, {Value: "bob@other.example"}}, PGPKey: expired, PGPKeySource: "qr", PGPKeyVerified: true}))
	kr := &keyResolver{store: store, settings: pgpdiscovery.Settings{StoreDiscoveredKeys: true}, discover: true}

	if rk := kr.resolve(context.Background(), "bob@example.com"); rk.Fingerprint != fingerprintOf(t, expired) {
		t.Fatalf("encryption target = %s, want the pinned v6 key", rk.Fingerprint)
	}
	c, _ := must2(findContact(store, "bob@example.com"))
	if c.PGPKeyFingerprint != fingerprintOf(t, k4) || c.PGPKeyV6Fingerprint != fingerprintOf(t, expired) || !c.PGPKeyV6Verified || c.PGPKeyV6Source != "qr" {
		t.Fatalf("contact = v4 %s v6 %s verified %v source %s", c.PGPKeyFingerprint, c.PGPKeyV6Fingerprint, c.PGPKeyV6Verified, c.PGPKeyV6Source)
	}
}
