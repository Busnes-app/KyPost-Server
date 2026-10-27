package pgpmail

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// ErrWeakRSAKey refuses new ciphertext or signatures from an RSA key below
// 2048 bits; go-crypto's own floor is 2047. Stored keys still parse, and
// history encrypted to them stays readable.
var ErrWeakRSAKey = errors.New("RSA keys below 2048 bits are refused for new encryption and signatures")

// requireStrongRSA checks the primary key and any key go-crypto selects for
// this operation, measuring the actual modulus. go-crypto already skips
// subkeys below 2047 bits and refuses anything else it cannot use.
func requireStrongRSA(key *crypto.Key, signing bool) error {
	entity := key.GetEntity()
	keys := []any{entity.PrimaryKey.PublicKey}
	selected, ok := entity.EncryptionKey(time.Now(), nil)
	if signing {
		selected, ok = entity.SigningKey(time.Now(), nil)
	}
	if ok {
		keys = append(keys, selected.PublicKey.PublicKey)
	}
	for _, pub := range keys {
		if rsaKey, isRSA := pub.(*rsa.PublicKey); isRSA && rsaKey.N.BitLen() < 2048 {
			return fmt.Errorf("pgpmail: key %s uses RSA-%d: %w", key.GetFingerprint(), rsaKey.N.BitLen(), ErrWeakRSAKey)
		}
	}
	return nil
}
