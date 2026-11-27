package pgpmail

import (
	"errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/armor"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"strings"
)

// InspectPQCKey narrows the public-key allowlist to the account's P4 profile.
func InspectPQCKey(armored string) (PublicKeyInfo, error) {
	trimmed := strings.TrimSpace(armored)
	if !strings.HasPrefix(trimmed, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.HasSuffix(trimmed, "-----END PGP PUBLIC KEY BLOCK-----") || strings.Count(trimmed, "-----BEGIN PGP ") != 1 {
		return PublicKeyInfo{}, errors.New("expected one public PQ certificate")
	}
	binary, err := armor.Unarmor(trimmed)
	if err != nil {
		return PublicKeyInfo{}, err
	}
	ring, err := crypto.NewKeyRingFromBinary(binary)
	if err != nil || ring.CountEntities() != 1 {
		return PublicKeyInfo{}, errors.New("expected one public PQ certificate")
	}
	info, err := InspectPublicKey(armored)
	if err != nil {
		return PublicKeyInfo{}, err
	}
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return PublicKeyInfo{}, err
	}
	e := key.GetEntity()
	status, err := CheckKeyStatus(armored)
	if err != nil || !status.Usable() || !info.CanEncrypt || e.PrimaryKey.Version != 6 ||
		e.PrimaryKey.PubKeyAlgo != packet.PubKeyAlgoMldsa65Ed25519 || len(e.Subkeys) != 1 ||
		e.Subkeys[0].PublicKey.PubKeyAlgo != packet.PubKeyAlgoMlkem768X25519 {
		return PublicKeyInfo{}, errors.New("expected a usable v6 ML-DSA-65+Ed25519 / ML-KEM-768+X25519 certificate")
	}
	return info, nil
}
