package pgpmailtest

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

func TestGenerateIdentity(t *testing.T) {
	id, err := GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if id.Fingerprint == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	if id.ArmoredPublicKey == "" {
		t.Fatal("expected non-empty armored public key")
	}
}

// userIDEmails returns the lowercased email of every User ID on an armored
// key, which is what the address-binding checks elsewhere in the codebase
// (validateDiscoveredKey, buildAutocryptHeader) match against.
func userIDEmails(t *testing.T, armored string) []string {
	t.Helper()
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		t.Fatalf("parse armored key: %v", err)
	}
	entity := key.GetEntity()
	if entity == nil {
		t.Fatal("key has no entity")
	}
	var out []string
	for _, uid := range entity.Identities {
		out = append(out, strings.ToLower(uid.UserId.Email))
	}
	sort.Strings(out)
	return out
}

func TestGenerateIdentityCarriesAdditionalAddressesAsUserIDs(t *testing.T) {
	id, err := GenerateIdentity("Alice", "alice@example.com", "alice@other.example", "sales@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	got := userIDEmails(t, id.ArmoredPublicKey)
	want := []string{"alice@example.com", "alice@other.example", "sales@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("user ID emails: got %v want %v", got, want)
	}
}

// TestGenerateIdentitySkipsDuplicateAndEmptyAdditionalAddresses covers the
// account address itself showing up again as an alias, and a blank entry.
// Neither may produce a duplicate or empty User ID.
func TestGenerateIdentitySkipsDuplicateAndEmptyAdditionalAddresses(t *testing.T) {
	id, err := GenerateIdentity("Alice", "alice@example.com", "ALICE@example.com", "  ", "alice@other.example", "alice@other.example")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	got := userIDEmails(t, id.ArmoredPublicKey)
	want := []string{"alice@example.com", "alice@other.example"}
	if !slices.Equal(got, want) {
		t.Fatalf("user ID emails: got %v want %v", got, want)
	}
}
