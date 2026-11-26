package pgpmail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// AutocryptKeydata returns the minimal key Autocrypt Level 1 §2.1.1 asks for:
// the primary key, the one User ID whose address is addr with its
// self-signature, and the current encryption subkey with its binding
// signature. A revoked or expired key has no valid self-signature or
// encryption key and is refused, so the minimal form never drops a
// revocation a recipient needs to see.
func AutocryptKeydata(armored, addr string) ([]byte, error) {
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: parse key: %w", err)
	}
	e := key.GetEntity()
	now := time.Now()
	target := strings.ToLower(strings.TrimSpace(addr))
	names := make([]string, 0, len(e.Identities))
	for name := range e.Identities {
		names = append(names, name)
	}
	slices.Sort(names) // one deterministic pick when several User IDs carry addr
	var uid *packet.UserId
	var uidSig *packet.Signature
	for _, name := range names {
		id := e.Identities[name]
		if strings.ToLower(strings.TrimSpace(id.UserId.Email)) != target {
			continue
		}
		if sig, err := id.Verify(now, nil); err == nil { // Verify also checks the User ID is not revoked
			uid, uidSig = id.UserId, sig
			break
		}
	}
	if uid == nil {
		return nil, fmt.Errorf("pgpmail: key does not carry %s as a user ID", addr)
	}
	enc, ok := e.EncryptionKey(now, nil)
	if !ok {
		return nil, errors.New("pgpmail: key has no usable encryption key")
	}
	parts := []interface{ Serialize(io.Writer) error }{e.PrimaryKey}
	if e.PrimaryKey.Version == 6 {
		// v6 keeps the key's properties in the direct-key signature.
		direct, err := e.LatestValidDirectSignature(now, nil)
		if err != nil {
			return nil, fmt.Errorf("pgpmail: v6 key without a direct-key signature: %w", err)
		}
		parts = append(parts, direct)
	}
	parts = append(parts, uid, uidSig)
	if enc.PublicKey != e.PrimaryKey {
		parts = append(parts, enc.PublicKey, enc.SelfSignature)
	}
	var out bytes.Buffer
	for _, p := range parts {
		if err := p.Serialize(&out); err != nil {
			return nil, fmt.Errorf("pgpmail: serialize key: %w", err)
		}
	}
	return out.Bytes(), nil
}
