package processor

import (
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// TestAutocryptHarvestAppliesKeyPolicy feeds the allowlist set to the harvest
// validator as the binary keydata an Autocrypt header carries.
func TestAutocryptHarvestAppliesKeyPolicy(t *testing.T) {
	const addr = "policy@example.org"
	keys, err := pgpmailtest.PolicyKeys(addr)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		block, err := armor.Decode(strings.NewReader(k.Armored))
		if err != nil {
			t.Fatal(err)
		}
		keydata, _ := io.ReadAll(block.Body)
		if _, _, err := validateAutocryptKey(keydata, addr); k.Allowed != (err == nil) {
			t.Errorf("%s: allowed=%v, got %v", k.Name, k.Allowed, err)
		}
	}
}
