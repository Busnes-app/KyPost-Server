// Package pgpmail implements OpenPGP encryption, signing, and key-identity
// management for the mail send/receive paths. All private key material is
// held in memory only for the duration of a request; callers persist it via
// SealPrivateKey (an AES-GCM envelope, the same pattern as
// mfa.SealTOTPSecret) and never write unsealed key material to disk.
package pgpmail

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

// Identity holds one OpenPGP keypair loaded in memory: a user's own private
// identity, used for decrypting and signing. Recipients' public keys are
// passed around as plain armored strings (Contact.PGPKey) since they never
// need decrypt/sign and so never need this type.
type Identity struct {
	Fingerprint      string
	KeyID            string
	ArmoredPublicKey string

	key *crypto.Key
}

func identityFromKey(key *crypto.Key) (*Identity, error) {
	armoredPub, err := key.GetArmoredPublicKey()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: armor public key: %w", err)
	}
	return &Identity{
		Fingerprint:      key.GetFingerprint(),
		KeyID:            key.GetHexKeyID(),
		ArmoredPublicKey: armoredPub,
		key:              key,
	}, nil
}

// SealPrivateKey AES-GCM seals the identity's private key with the master
// key at keyPath (creating the key on first use) and returns the JSON
// envelope as a string, ready to store on User.PGPPrivateKeyEnc. The armored
// form stored inside the envelope is unprotected (no passphrase) — the
// envelope's AES-GCM key is the sole protection, matching how
// mfa.SealTOTPSecret protects TOTP secrets.
func (id *Identity) SealPrivateKey(keyPath string) (string, error) {
	armored, err := id.key.Armor()
	if err != nil {
		return "", fmt.Errorf("pgpmail: armor private key: %w", err)
	}
	return cryptutil.SealString(armored, keyPath)
}

// OpenPrivateKey reverses SealPrivateKey, returning a usable Identity.
// Mirrors mfa.OpenTOTPSecret.
func OpenPrivateKey(enc, keyPath string) (*Identity, error) {
	armored, err := cryptutil.OpenString(enc, keyPath, errors.New("pgpmail: private key is not a valid envelope"))
	if err != nil {
		return nil, err
	}
	return ParsePrivateKey(armored)
}

// ParsePrivateKey loads an unprotected armored private key, as unsealed by
// OpenPrivateKey.
func ParsePrivateKey(armored string) (*Identity, error) {
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: parse stored private key: %w", err)
	}
	return identityFromKey(key)
}

// PublicKeyInfo describes an armored public key well enough to store it as
// a user's identity, without ever seeing the matching private key.
type PublicKeyInfo struct {
	CanEncrypt      bool
	KeyFingerprints []string
	Fingerprint     string
	KeyID           string
	// ArmoredPublicKey is the re-armored key, not the caller's input: the
	// stored form is whatever this library produces from the parsed key, so
	// a client cannot smuggle trailing data past the parser by wrapping it
	// in an armor block the parser tolerates.
	ArmoredPublicKey string
}

// InspectPublicKey parses an armored public key and reports its identifying
// fields.
//
// This exists for the client-protected key flow: the browser generates the
// keypair, wraps the private half itself, and uploads only the public half
// plus an opaque wrapped blob. The server must therefore derive the
// fingerprint and key ID from the key it was actually given rather than
// believing whatever the client claimed them to be — a client that could
// assert an arbitrary fingerprint could get its own key published under
// another key's identity via WKD or Autocrypt.
func InspectPublicKey(armoredPublicKey string) (PublicKeyInfo, error) {
	key, err := crypto.NewKeyFromArmored(strings.TrimSpace(armoredPublicKey))
	if err != nil {
		return PublicKeyInfo{}, fmt.Errorf("pgpmail: parse public key: %w", err)
	}
	if key.IsPrivate() {
		// A private key here means the client uploaded the wrong half. Refuse
		// rather than quietly storing a private key in a public field.
		return PublicKeyInfo{}, errors.New("pgpmail: expected a public key, got a private key")
	}
	armored, err := key.GetArmoredPublicKey()
	if err != nil {
		return PublicKeyInfo{}, fmt.Errorf("pgpmail: armor public key: %w", err)
	}
	fingerprints := []string{strings.ToUpper(key.GetFingerprint())}
	for _, subkey := range key.GetEntity().Subkeys {
		fingerprints = append(fingerprints, strings.ToUpper(hex.EncodeToString(subkey.PublicKey.Fingerprint)))
	}
	return PublicKeyInfo{
		CanEncrypt:       key.CanEncrypt(time.Now().Unix()),
		KeyFingerprints:  fingerprints,
		Fingerprint:      key.GetFingerprint(),
		KeyID:            key.GetHexKeyID(),
		ArmoredPublicKey: armored,
	}, nil
}

// ExportArmoredPrivateKey returns the identity's armored private key.
//
// The only caller is the one-time migration that hands a legacy
// server-sealed key back to its owner's browser so the browser can rewrap it
// under a key the server does not have. It is deliberately a distinct,
// awkwardly-named method rather than a field: exporting a private key is
// not something any other code path should reach for by accident.
func (id *Identity) ExportArmoredPrivateKey() (string, error) {
	armored, err := id.key.Armor()
	if err != nil {
		return "", fmt.Errorf("pgpmail: armor private key: %w", err)
	}
	return armored, nil
}
