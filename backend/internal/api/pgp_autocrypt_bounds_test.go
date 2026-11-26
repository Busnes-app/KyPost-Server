package api

import (
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpautocrypt"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// TestBuildAutocryptHeaderIsMinimal: three User IDs and two encryption
// subkeys go in; the header carries the From User ID and one subkey, and
// still validates as a usable key for that address.
func TestBuildAutocryptHeaderIsMinimal(t *testing.T) {
	id, err := pgpmailtest.GenerateIdentity("Bob", "bob@example.com", "bob@other.example", "b@third.example")
	if err != nil {
		t.Fatal(err)
	}
	full, err := crypto.NewKeyFromArmored(id.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	header, ok := buildAutocryptHeader(id.ArmoredPublicKey, "bob@other.example")
	if !ok {
		t.Fatal("no header")
	}
	_, keydata, err := pgpautocrypt.ParseAutocryptHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewKey(keydata)
	if err != nil {
		t.Fatal(err)
	}
	e := key.GetEntity()
	if len(e.Identities) != 1 || len(e.Subkeys) != 1 || key.GetFingerprint() != full.GetFingerprint() {
		t.Fatalf("got %d user IDs and %d subkeys, want 1 and 1 of the same key", len(e.Identities), len(e.Subkeys))
	}
	for _, uid := range e.Identities {
		if uid.UserId.Email != "bob@other.example" {
			t.Fatalf("kept user ID %q, want the From address", uid.UserId.Email)
		}
	}
	if _, err := validateDiscoveredKey(mustArmor(t, key), "bob@other.example"); err != nil {
		t.Fatalf("minimal key is not usable: %v", err)
	}
	// A v4 key with an ML-KEM-768+X25519 subkey still carries one subkey.
	pqc := string(must1(os.ReadFile("../../../testdata/openpgp-v6/v4-pqc.pub.asc")))
	header, ok = buildAutocryptHeader(pqc, "v4-pqc@example.org")
	if !ok {
		t.Fatal("no header for the v4 key with an ID 35 subkey")
	}
	_, keydata, _ = pgpautocrypt.ParseAutocryptHeader(header)
	if key, err = crypto.NewKey(keydata); err != nil || len(key.GetEntity().Subkeys) != 1 {
		t.Fatalf("v4+ID 35 header: %v", err)
	}
}

func mustArmor(t *testing.T, key *crypto.Key) string {
	t.Helper()
	armored, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

// TestBuildAutocryptHeaderRefusesUnusableAndOversizeKeys: a revoked key is not
// advertised (minimizing would drop its revocation), nor is a key over 10 KiB.
func TestBuildAutocryptHeaderRefusesUnusableAndOversizeKeys(t *testing.T) {
	if _, ok := buildAutocryptHeader(generateRevokedArmoredKey(t, "Bob", "bob@example.com"), "bob@example.com"); ok {
		t.Fatal("revoked key advertised")
	}
	v6 := string(must1(os.ReadFile("../../../testdata/openpgp-v6/v6-pqc.pub.asc")))
	if _, ok := buildAutocryptHeader(v6, "v6-pqc@example.org"); ok {
		t.Fatal("13.6 KB v6 PQC key advertised past the 10 KiB cap")
	}
	if header, ok := buildAutocryptHeader(string(must1(os.ReadFile("../../../testdata/openpgp-v6/v6.pub.asc"))), "v6@example.org"); !ok || len(header) > pgpautocrypt.MaxHeaderBytes || !strings.HasPrefix(header, "addr=v6@example.org;") {
		t.Fatalf("v6 Ed25519 key: ok=%v len=%d", ok, len(header))
	}
}
