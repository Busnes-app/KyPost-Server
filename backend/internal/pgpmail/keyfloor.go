package pgpmail

import (
	"fmt"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpprofile"
)

// ErrWeakRSAKey refuses new ciphertext or signatures from an RSA key below
// 2048 bits. The check lives in pgpprofile so the browser's wasm engine runs
// the same code.
var ErrWeakRSAKey = pgpprofile.ErrWeakRSAKey

func requireStrongRSA(key *crypto.Key, signing bool) error {
	if err := pgpprofile.RequireStrongRSA(key, signing); err != nil {
		return fmt.Errorf("pgpmail: %w", err)
	}
	return nil
}
