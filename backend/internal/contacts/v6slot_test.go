package contacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

func v4Key(t *testing.T, email string) string {
	t.Helper()
	id, err := pgpmailtest.GenerateIdentity("Bob", email)
	if err != nil {
		t.Fatal(err)
	}
	return id.ArmoredPublicKey
}

func v6Key(t *testing.T, email string, pqc bool) string {
	t.Helper()
	k, err := pgpmailtest.GenerateV6PublicKey("Bob", email, pqc)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A correspondent adding a v6 certificate keeps the pinned v4 key, pin and
// confirmation; the v6 key gets its own slot and pin.
func TestV6KeyAddedBesideV4KeepsV4Pin(t *testing.T) {
	store := must(New(t.TempDir()))
	k4, k6 := v4Key(t, "bob@example.com"), v6Key(t, "bob@example.com", true)
	c := must(store.Upsert(Contact{FormattedName: "Bob", Emails: []ContactValue{{Value: "bob@example.com"}}, PGPKey: k4, PGPKeySource: PGPSourceManual, PGPKeyVerified: true}))
	pin4 := c.PGPKeyFingerprint

	// An old client sends one pgpKey, echoing the v4 pin it read.
	c.PGPKey, c.PGPKeySource = k6, PGPSourceWKD
	got := must(store.Upsert(c))
	if got.PGPKeyFingerprint != pin4 || pgpKeyFingerprint(got.PGPKey) != pin4 || !got.PGPKeyVerified || got.PGPKeySource != PGPSourceManual {
		t.Fatalf("v4 slot changed: fp=%s verified=%v source=%s", got.PGPKeyFingerprint, got.PGPKeyVerified, got.PGPKeySource)
	}
	if got.PGPKeyV6 == "" || got.PGPKeyV6Fingerprint != pgpKeyFingerprint(k6) || got.PGPKeyV6Verified {
		t.Fatalf("v6 slot = %v fp=%s verified=%v", got.PGPKeyV6 != "", got.PGPKeyV6Fingerprint, got.PGPKeyV6Verified)
	}

	// Writers that know nothing of the slot keep it by omission.
	got.PGPKeyV6, got.PGPKeyV6Fingerprint = "", ""
	again := must(store.Upsert(got))
	if again.PGPKeyV6Fingerprint != pgpKeyFingerprint(k6) {
		t.Fatal("v6 slot dropped by a write that omitted it")
	}

	// Confirming the v6 fingerprint confirms the v6 slot only.
	conf, _, err := store.ConfirmPGPKey(again.UID, again.PGPKeyV6Fingerprint)
	if err != nil || !conf.PGPKeyV6Verified {
		t.Fatalf("confirm v6: %v %v", err, conf.PGPKeyV6Verified)
	}

	// An aware client dropping only the v4 key keeps the v6 key as the only one.
	only6 := conf
	only6.PGPKey = ""
	if kept := must(store.Upsert(only6)); kept.PGPKeyFingerprint != pgpKeyFingerprint(k6) || kept.PGPKeyV6 != "" {
		t.Fatal("dropping the v4 key did not leave the v6 key as the only key")
	}

	// An old client removing "the key" (it sends no v6 slot) removes both.
	conf = must(store.Upsert(conf))
	conf.PGPKey, conf.PGPKeyV6 = "", ""
	gone := must(store.Upsert(conf))
	if gone.PGPKey != "" || gone.PGPKeyV6 != "" || gone.PGPKeyV6Fingerprint != "" {
		t.Fatal("removing the key left the v6 key behind")
	}
}

// A different v4 key is still a key change: the v4 slot is replaced and the
// old pin is NOT moved onto it by the v6 slot logic.
func TestDifferentV4KeyIsStillAChange(t *testing.T) {
	store := must(New(t.TempDir()))
	k4 := v4Key(t, "bob@example.com")
	c := must(store.Upsert(Contact{FormattedName: "Bob", PGPKey: k4}))
	pin := c.PGPKeyFingerprint
	other := v4Key(t, "bob@example.com")
	c.PGPKey = other
	got := must(store.Upsert(c))
	if got.PGPKeyV6 != "" || pgpKeyFingerprint(got.PGPKey) == pin {
		t.Fatal("a second v4 key was not treated as the v4 slot")
	}
	// The writer echoed the old pin, so the stored key disputes it.
	if got.PGPKeyFingerprint != pin {
		t.Fatalf("pin = %s, want the echoed old pin %s", got.PGPKeyFingerprint, pin)
	}
}

// A contacts.json written before the slot existed reads its single pgpKey as
// the original (v4) slot, unchanged.
func TestOldSingleKeyFormatReadsAsV4Slot(t *testing.T) {
	dir := t.TempDir()
	k4 := v4Key(t, "bob@example.com")
	raw, _ := json.Marshal(map[string]any{"contacts": []map[string]any{{
		"uid": "u1", "rev": 1, "fn": "Bob", "pgpKey": k4, "pgpKeyFingerprint": pgpKeyFingerprint(k4), "pgpKeySource": "manual",
	}}, "seq": 1})
	if err := os.WriteFile(filepath.Join(dir, "contacts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store := must(New(dir))
	c, ok := mustGet(t, store, "u1")
	if !ok || c.PGPKey != k4 || c.PGPKeyFingerprint != pgpKeyFingerprint(k4) || c.PGPKeyV6 != "" {
		t.Fatalf("old record did not read as the v4 slot: %+v", c)
	}
	// And a v6 key added to it lands beside it.
	c.PGPKey = v6Key(t, "bob@example.com", false)
	got := must(store.Upsert(c))
	if got.PGPKey != k4 || got.PGPKeyV6 == "" {
		t.Fatal("v6 key did not land in the v6 slot of an old record")
	}
}

func TestV6SlotRefusesNonV6Key(t *testing.T) {
	store := must(New(t.TempDir()))
	_, err := store.Upsert(Contact{FormattedName: "Bob", PGPKey: v4Key(t, "b@example.com"), PGPKeyV6: v4Key(t, "b@example.com")})
	if err == nil || !strings.Contains(err.Error(), "v6") {
		t.Fatalf("err = %v, want ErrV6Slot", err)
	}
	// A v6 slot alone becomes the only key.
	got := must(store.Upsert(Contact{FormattedName: "Bob", PGPKeyV6: v6Key(t, "b@example.com", false)}))
	if got.PGPKey == "" || got.PGPKeyV6 != "" {
		t.Fatal("a lone v6 slot did not move into pgpKey")
	}
}

// Adding the v6 key changes what the address book anchors, so it bumps the
// generation consumers invalidate on.
func TestV6SlotBumpsKeyGeneration(t *testing.T) {
	store := must(New(t.TempDir()))
	c := must(store.Upsert(Contact{FormattedName: "Bob", PGPKey: v4Key(t, "b@example.com")}))
	before := mustPGPKeyGeneration(t, store)
	c.PGPKey = v6Key(t, "b@example.com", false)
	must(store.Upsert(c))
	if mustPGPKeyGeneration(t, store) == before {
		t.Fatal("adding a v6 key did not bump the key generation")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// An Autocrypt-harvested v6 key over a v4 key is not an addition: Autocrypt
// carries no v6 certificate, so the write keeps the old pin and reads as the
// key change it is.
func TestAutocryptV6OverV4StaysAKeyChange(t *testing.T) {
	store := must(New(t.TempDir()))
	k4 := v4Key(t, "bob@example.com")
	c := must(store.Upsert(Contact{FormattedName: "Bob", PGPKey: k4, PGPKeySource: PGPSourceAutocrypt}))
	pin := c.PGPKeyFingerprint
	c.PGPKey = v6Key(t, "bob@example.com", false) // the harvest keeps source and old pin
	got := must(store.Upsert(c))
	if got.PGPKeyV6 != "" || got.PGPKeyFingerprint != pin || pgpKeyFingerprint(got.PGPKey) == pin {
		t.Fatalf("autocrypt v6 moved into the v6 slot: v6=%v pin=%s", got.PGPKeyV6 != "", got.PGPKeyFingerprint)
	}
}
