// Package pgpprofile holds the OpenPGP choices every KyPost engine shares: the
// server (internal/pgpmail) and the browser's gopenpgp WebAssembly build
// (cmd/pgpwasm). It imports nothing but gopenpgp so the wasm stays small.
package pgpprofile

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/ProtonMail/gopenpgp/v3/profile"
)

// ErrWeakRSAKey refuses new ciphertext or signatures from an RSA key below
// 2048 bits; go-crypto's own floor is 2047. Stored keys still parse, and
// history encrypted to them stays readable.
var ErrWeakRSAKey = errors.New("RSA keys below 2048 bits are refused for new encryption and signatures")

// V6 is the KyPost v6 profile (spec A, "The KyPost profile"): gopenpgp's
// PQC preset (ML-DSA-65+Ed25519 primary, ML-KEM-768+X25519 subkey, AES-256,
// Argon2) with AEAD OCB, RFC 9580's mandatory mode, instead of GCM.
func V6() *crypto.PGPHandle {
	p := profile.PQC()
	p.AeadEncryption = &packet.AEADConfig{DefaultMode: packet.AEADModeOCB}
	p.AeadKeyEncryption = &packet.AEADConfig{DefaultMode: packet.AEADModeOCB}
	return crypto.PGPWithProfile(p)
}

// RequireStrongRSA checks the primary key and any key go-crypto selects for
// this operation, measuring the actual modulus. go-crypto already skips
// subkeys below 2047 bits and refuses anything else it cannot use.
func RequireStrongRSA(key *crypto.Key, signing bool) error {
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
			return fmt.Errorf("key %s uses RSA-%d: %w", key.GetFingerprint(), rsaKey.N.BitLen(), ErrWeakRSAKey)
		}
	}
	return nil
}
