package contacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func privateTestKey(t *testing.T, addr string) (armored, fingerprint string) {
	t.Helper()
	key, err := crypto.PGP().KeyGeneration().AddUserId("Test", addr).New().GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	armored, err = key.Armor()
	if err != nil {
		t.Fatalf("Armor: %v", err)
	}
	return armored, key.GetFingerprint()
}

func assertPublicOnly(t *testing.T, armored, wantFP string) {
	t.Helper()
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		t.Fatalf("stored key does not parse: %v", err)
	}
	if key.IsPrivate() || strings.Contains(armored, "PRIVATE") {
		t.Fatal("secret key material was stored")
	}
	if key.GetFingerprint() != wantFP {
		t.Fatalf("stored fingerprint %s, want %s", key.GetFingerprint(), wantFP)
	}
}

// Whatever writer hands the store a private key, only its public half lands.
func TestUpsertStoresOnlyThePublicKey(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	priv, fp := privateTestKey(t, "bob@example.com")
	c, err := store.Upsert(Contact{FormattedName: "Bob", Emails: []ContactValue{{Value: "bob@example.com"}}, PGPKey: priv})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	stored, _ := mustGet(t, store, c.UID)
	assertPublicOnly(t, stored.PGPKey, fp)
	if stored.PGPKeyFingerprint != fp {
		t.Fatalf("pin = %q, want %q", stored.PGPKeyFingerprint, fp)
	}
}

// Contacts stored before the sink stripped keys are cleaned on open: once,
// keeping the pin and provenance, and a second open changes nothing.
func TestOpenStripsStoredPrivateKeysIdempotently(t *testing.T) {
	dir := t.TempDir()
	priv, fp := privateTestKey(t, "bob@example.com")
	legacy := contactsFile{Seq: 1, Contacts: []Contact{{
		UID: "u1", Rev: 1, FormattedName: "Bob", Emails: []ContactValue{{Value: "bob@example.com"}},
		PGPKey: priv, PGPKeyFingerprint: fp, PGPKeySource: PGPSourceManual, PGPKeyVerified: true,
	}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contacts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, _ := mustGet(t, store, "u1")
	assertPublicOnly(t, first.PGPKey, fp)
	if first.PGPKeyFingerprint != fp || first.PGPKeySource != PGPSourceManual || !first.PGPKeyVerified {
		t.Fatalf("migration dropped provenance: %+v", first)
	}
	if first.Rev == 1 {
		t.Fatal("migration must bump Rev so sync clients re-fetch the public key")
	}

	again, err := New(dir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	second, _ := mustGet(t, again, "u1")
	if second.Rev != first.Rev || second.PGPKey != first.PGPKey {
		t.Fatalf("second run changed the contact: rev %d -> %d", first.Rev, second.Rev)
	}
}

// The same key in a different armoring is the same key: the pin and
// provenance carry, and a pin the stored key disputes is not overwritten by
// one derived from the incoming bytes.
func TestUpsertCarriesProvenanceForTheSameKeyReArmored(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	priv, fp := privateTestKey(t, "bob@example.com")
	seeded, err := store.Upsert(Contact{
		FormattedName: "Bob", Emails: []ContactValue{{Value: "bob@example.com"}},
		PGPKey: priv, PGPKeyFingerprint: "DISPUTED-PIN", PGPKeySource: PGPSourceAutocrypt,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Resend the original (private) armoring, as a stale client would.
	updated, err := store.Upsert(Contact{UID: seeded.UID, FormattedName: "Bob", Emails: seeded.Emails, PGPKey: priv})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if updated.PGPKeyFingerprint != "DISPUTED-PIN" || updated.PGPKeySource != PGPSourceAutocrypt {
		t.Fatalf("resending the same key re-pinned it: %+v", updated)
	}
	assertPublicOnly(t, updated.PGPKey, fp)
}

// A CardDAV PUT that drops KEY keeps the stored key and pin.
func TestKeepAbsentPGPKeyKeepsKeyAndPin(t *testing.T) {
	store, seeded := seedPinnedContact(t)

	updated, err := store.UpsertWithPrecondition(Contact{UID: seeded.UID, FormattedName: "Bob Phone"},
		ContactPrecondition{KeepAbsentPGPKey: true})
	if err != nil {
		t.Fatalf("UpsertWithPrecondition: %v", err)
	}
	if updated.PGPKey != testArmoredKey || updated.PGPKeyFingerprint != "AAAA1111BBBB2222" || !updated.PGPKeyVerified {
		t.Fatalf("absent KEY wiped the key or pin: %+v", updated)
	}
	if updated.FormattedName != "Bob Phone" {
		t.Fatalf("the rest of the write did not land: %q", updated.FormattedName)
	}
}
