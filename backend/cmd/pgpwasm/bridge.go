//go:build js && wasm

package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/armor"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpprofile"
)

// The bridge is deliberately thin: every operation is one gopenpgp call with
// the profile the server uses. Inputs are armored or binary OpenPGP bytes;
// outputs are armored, matching what pgpClient.ts already stores and sends.

// errPassphraseRequired is matched by pgpEngineWasm.ts, which shows the
// same prompt as the openpgp.js path.
var errPassphraseRequired = errors.New("passphrase required")

type userID struct{ Name, Email string }

type keyInfo struct {
	Fingerprint  string   // primary, upper-case hex
	Version      int      // 4 or 6
	IsPrivate    bool     // carries secret material
	KeyIDs       []string // primary then subkeys, upper-case hex
	Fingerprints []string // same order
	Algorithms   []int    // same order, RFC 9580 / RFC 9980 IDs
}

type signatureCheck struct {
	KeyID       string // upper-case hex, "" when the signature names only a fingerprint
	Fingerprint string // upper-case hex issuer fingerprint, "" when absent
	Valid       bool
}

func handleFor(version int) *crypto.PGPHandle {
	if version == 6 {
		return pgpprofile.V6()
	}
	return crypto.PGP()
}

func generate(profileName string, ids []userID) (privateArmor, publicArmor, fingerprint string, err error) {
	if len(ids) == 0 {
		return "", "", "", errors.New("generate: at least one user ID")
	}
	var handle *crypto.PGPHandle
	switch profileName {
	case "v4":
		handle = crypto.PGP() // v4 EdDSA-legacy + cv25519, what openpgp.js curve25519Legacy makes
	case "v6":
		handle = pgpprofile.V6()
	default:
		return "", "", "", fmt.Errorf("generate: unknown profile %q", profileName)
	}
	b := handle.KeyGeneration()
	for _, id := range ids {
		b = b.AddUserId(id.Name, id.Email)
	}
	key, err := b.New().GenerateKey()
	if err != nil {
		return "", "", "", err
	}
	defer key.ClearPrivateParams()
	if privateArmor, err = key.Armor(); err != nil {
		return "", "", "", err
	}
	if publicArmor, err = key.GetArmoredPublicKey(); err != nil {
		return "", "", "", err
	}
	return privateArmor, publicArmor, strings.ToUpper(key.GetFingerprint()), nil
}

// importPrivate returns the key unlocked: pgpClient wraps it under the account
// password instead, exactly as the openpgp.js path does.
func importPrivate(data, passphrase []byte) (privateArmor, publicArmor, fingerprint string, err error) {
	key, err := crypto.NewKey(data)
	if err != nil {
		return "", "", "", err
	}
	if !key.IsPrivate() {
		return "", "", "", errors.New("import: not a private key")
	}
	locked, err := key.IsLocked()
	if err != nil {
		return "", "", "", err
	}
	if locked {
		if len(passphrase) == 0 {
			return "", "", "", errPassphraseRequired
		}
		unlocked, err := key.Unlock(passphrase)
		if err != nil {
			return "", "", "", err
		}
		key.ClearPrivateParams()
		key = unlocked
	}
	defer key.ClearPrivateParams()
	if !locked && bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----")) {
		// Nothing to unlock: keep the user's bytes rather than re-serializing.
		privateArmor = string(data)
	} else if privateArmor, err = key.Armor(); err != nil {
		return "", "", "", err
	}
	if publicArmor, err = key.GetArmoredPublicKey(); err != nil {
		return "", "", "", err
	}
	return privateArmor, publicArmor, strings.ToUpper(key.GetFingerprint()), nil
}

func inspect(data []byte) (keyInfo, error) {
	key, err := crypto.NewKey(data)
	if err != nil {
		return keyInfo{}, err
	}
	defer key.ClearPrivateParams()
	e := key.GetEntity()
	info := keyInfo{Fingerprint: strings.ToUpper(key.GetFingerprint()), Version: key.GetVersion(), IsPrivate: key.IsPrivate()}
	add := func(pk *packet.PublicKey) {
		info.KeyIDs = append(info.KeyIDs, fmt.Sprintf("%016X", pk.KeyId))
		info.Fingerprints = append(info.Fingerprints, fmt.Sprintf("%X", pk.Fingerprint))
		info.Algorithms = append(info.Algorithms, int(pk.PubKeyAlgo))
	}
	add(e.PrimaryKey)
	for _, sub := range e.Subkeys {
		add(sub.PublicKey)
	}
	return info, nil
}

// loadKeys parses every key into one ring. A delivery group never mixes key
// versions (spec A, "Mixed recipients"), so writers pass oneVersion and get
// the version back; a reading ring legitimately holds v4 and v6 keys.
func loadKeys(list [][]byte, private, oneVersion bool) (*crypto.KeyRing, int, error) {
	ring, err := crypto.NewKeyRing(nil)
	if err != nil {
		return nil, 0, err
	}
	version := 0
	for _, data := range list {
		key, err := crypto.NewKey(data)
		if err != nil {
			return nil, 0, err
		}
		if private && !key.IsPrivate() {
			return nil, 0, errors.New("not a private key")
		}
		if oneVersion && version != 0 && key.GetVersion() != version {
			return nil, 0, errors.New("one message cannot mix v4 and v6 keys; split the delivery group")
		}
		version = key.GetVersion()
		if err := ring.AddKey(key); err != nil {
			return nil, 0, err
		}
	}
	return ring, version, nil
}

func requireStrong(ring *crypto.KeyRing, signing bool) error {
	for _, key := range ring.GetKeys() {
		if err := pgpprofile.RequireStrongRSA(key, signing); err != nil {
			return err
		}
	}
	return nil
}

func encrypt(data []byte, recipients [][]byte, signingKey []byte) (string, error) {
	if len(recipients) == 0 {
		return "", errors.New("encrypt: no recipient keys")
	}
	ring, version, err := loadKeys(recipients, false, true)
	if err != nil {
		return "", err
	}
	if err := requireStrong(ring, false); err != nil {
		return "", err
	}
	// Utf8: a text literal with canonical CRLF, as openpgp.js createMessage({text}).
	b := handleFor(version).Encryption().Recipients(ring).Utf8()
	if len(signingKey) > 0 {
		signer, signerVersion, err := loadKeys([][]byte{signingKey}, true, true)
		if err != nil {
			return "", err
		}
		defer signer.ClearPrivateParams()
		if signerVersion != version {
			return "", errors.New("encrypt: sign with the key that matches the delivery's version")
		}
		if err := requireStrong(signer, true); err != nil {
			return "", err
		}
		b = b.SigningKeys(signer)
	}
	h, err := b.New()
	if err != nil {
		return "", err
	}
	defer h.ClearPrivateParams()
	msg, err := h.Encrypt(data)
	if err != nil {
		return "", err
	}
	return msg.Armor()
}

func signDetached(data, signingKey []byte) (string, int, error) {
	signer, version, err := loadKeys([][]byte{signingKey}, true, true)
	if err != nil {
		return "", 0, err
	}
	defer signer.ClearPrivateParams()
	if err := requireStrong(signer, true); err != nil {
		return "", 0, err
	}
	h, err := handleFor(version).Sign().SigningKeys(signer).Detached().New()
	if err != nil {
		return "", 0, err
	}
	defer h.ClearPrivateParams()
	sig, err := h.Sign(data, crypto.Bytes)
	if err != nil {
		return "", 0, err
	}
	p, err := packet.Read(bytes.NewReader(sig))
	if err != nil {
		return "", 0, err
	}
	s, ok := p.(*packet.Signature)
	if !ok {
		return "", 0, errors.New("sign: no signature packet")
	}
	armored, err := armor.ArmorPGPSignature(sig)
	return armored, int(hashID(s)), err
}

// hashID maps Go's crypto.Hash back to its RFC 9580 section 9.5 ID.
func hashID(s *packet.Signature) byte {
	id, _ := map[string]byte{"SHA-256": 8, "SHA-384": 9, "SHA-512": 10, "SHA-224": 11, "SHA3-256": 12, "SHA3-512": 14}[s.Hash.String()]
	return id
}

func signatures(res *crypto.VerifyResult) []signatureCheck {
	if res == nil {
		return nil
	}
	out := make([]signatureCheck, 0, len(res.Signatures))
	for _, vs := range res.Signatures {
		// The RSA-2048 floor also governs signature trust, as in openpgp.js.
		c := signatureCheck{Valid: vs.SignatureError == nil && vs.SignedBy != nil && pgpprofile.RequireStrongRSA(vs.SignedBy, true) == nil}
		if vs.Signature != nil {
			if vs.Signature.IssuerKeyId != nil {
				c.KeyID = fmt.Sprintf("%016X", *vs.Signature.IssuerKeyId)
			}
			c.Fingerprint = fmt.Sprintf("%X", vs.Signature.IssuerFingerprint)
		}
		out = append(out, c)
	}
	return out
}

func decrypt(message []byte, decryptionKeys, verificationKeys [][]byte, maxBytes int64) ([]byte, []signatureCheck, error) {
	keys, _, err := loadKeys(decryptionKeys, true, false)
	if err != nil {
		return nil, nil, err
	}
	defer keys.ClearPrivateParams()
	verifiers, _, err := loadKeys(verificationKeys, false, false)
	if err != nil {
		return nil, nil, err
	}
	// An empty verification ring still reports each signature (unverified),
	// so the reader can say "signed, cannot check" as openpgp.js does.
	h, err := crypto.PGP().Decryption().DecryptionKeys(keys).VerificationKeys(verifiers).
		MaxDecompressedMessageSize(maxBytes).New()
	if err != nil {
		return nil, nil, err
	}
	defer h.ClearPrivateParams()
	res, err := h.Decrypt(message, crypto.Auto)
	if err != nil {
		return nil, nil, err
	}
	data := res.Bytes()
	if res.Metadata().IsUtf8() {
		// openpgp.js turns a text literal's canonical CRLF back into LF.
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	return data, signatures(&res.VerifyResult), nil
}

func verifyDetached(data, signature []byte, verificationKeys [][]byte) ([]signatureCheck, error) {
	verifiers, _, err := loadKeys(verificationKeys, false, false)
	if err != nil {
		return nil, err
	}
	h, err := crypto.PGP().Verify().VerificationKeys(verifiers).New()
	if err != nil {
		return nil, err
	}
	res, err := h.VerifyDetached(data, signature, crypto.Auto)
	if err != nil {
		return nil, err
	}
	return signatures(res), nil
}

func armorBytes(data []byte, armorType string) (string, error) {
	return armor.ArmorWithType(data, armorType)
}

func dearmor(data []byte) ([]byte, error) {
	return armor.UnarmorBytes(data)
}
