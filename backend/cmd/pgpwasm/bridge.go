//go:build js && wasm

package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	openpgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
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
	EncryptionAlgorithm int      // currently selected encryption subkey, 0 when unusable
	Fingerprint         string   // primary, upper-case hex
	Version             int      // 4 or 6
	IsPrivate           bool     // carries secret material
	Locked              bool     // some secret packet is encrypted or a GNU dummy
	Count               int      // keys in the input; only the first is described
	Expires             int64    // Unix seconds, 0 for none
	PublicKey           []byte   // binary transferable public key
	Packets             [][]byte // PublicKey split into packets, for an order-free comparison
	KeyIDs              []string // primary then subkeys, upper-case hex
	Fingerprints        []string // same order
	Algorithms          []int    // same order, RFC 9580 / RFC 9980 IDs
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
	ring, err := crypto.NewKeyRingFromBinary(bytesOrUnarmored(data))
	if err != nil {
		return keyInfo{}, err
	}
	pub, err := key.GetPublicKey()
	if err != nil {
		return keyInfo{}, err
	}
	packets, err := splitPackets(pub)
	if err != nil {
		return keyInfo{}, err
	}
	e := key.GetEntity()
	info := keyInfo{Packets: packets, Fingerprint: strings.ToUpper(key.GetFingerprint()), Version: key.GetVersion(), IsPrivate: key.IsPrivate(),
		Count: ring.CountEntities(), PublicKey: pub}
	if encryption, ok := e.EncryptionKey(time.Now(), nil); ok {
		info.EncryptionAlgorithm = int(encryption.PublicKey.PubKeyAlgo)
	}
	if key.IsPrivate() {
		locked, err := key.IsLocked()
		if err != nil {
			return keyInfo{}, err
		}
		// IsLocked is "every secret packet is encrypted"; a ring needs none to be.
		unusable := func(k *packet.PrivateKey) bool { return k == nil || k.Encrypted || k.Dummy() }
		info.Locked = locked || unusable(e.PrivateKey)
		for _, sub := range e.Subkeys {
			info.Locked = info.Locked || unusable(sub.PrivateKey)
		}
	}
	if sig, err := e.PrimarySelfSignature(time.Now(), nil); err == nil && sig.KeyLifetimeSecs != nil && *sig.KeyLifetimeSecs > 0 {
		info.Expires = e.PrimaryKey.CreationTime.Unix() + int64(*sig.KeyLifetimeSecs)
	}
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

func intendedRecipientMatchesRing(recipients []*packet.Recipient, ring *crypto.KeyRing) bool {
	if ring == nil || len(recipients) == 0 {
		return true
	}
	for _, key := range ring.GetKeys() {
		entity := key.GetEntity()
		if entity == nil || entity.PrimaryKey == nil {
			continue
		}
		for _, rec := range recipients {
			if bytes.Equal(rec.Fingerprint, entity.PrimaryKey.Fingerprint) {
				return true
			}
		}
	}
	return false
}

func signatures(res *crypto.VerifyResult, decryptionKeys *crypto.KeyRing) []signatureCheck {
	if res == nil {
		return nil
	}
	out := make([]signatureCheck, 0, len(res.Signatures))
	for _, vs := range res.Signatures {
		// The RSA-2048 floor also governs signature trust, as in openpgp.js.
		// For encrypted+signed messages re-encrypted from v4 to v6 (Spec A P6),
		// IntendedRecipients names the original v4 certificate that is still in
		// the account's own decryption ring; matching any key in decryptionKeys
		// preserves anti-surreptitious-forwarding while keeping v4 signatures valid.
		intendedOK := vs.Signature == nil || intendedRecipientMatchesRing(vs.Signature.IntendedRecipients, decryptionKeys)
		c := signatureCheck{Valid: vs.SignatureError == nil && vs.SignedBy != nil && intendedOK && pgpprofile.RequireStrongRSA(vs.SignedBy, true) == nil}
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
	if maxBytes < 1 {
		return nil, nil, errors.New("decrypt: invalid plaintext size limit")
	}
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
	// DisableIntendedRecipients delegates recipient checking to signatures()
	// across the account's full decryption ring so P6-reencrypted v4 mail keeps
	// valid inner signatures without weakening anti-forwarding protection.
	h, err := crypto.PGP().Decryption().DecryptionKeys(keys).VerificationKeys(verifiers).
		DisableIntendedRecipients().MaxDecompressedMessageSize(maxBytes).New()
	if err != nil {
		return nil, nil, err
	}
	defer h.ClearPrivateParams()
	res, err := h.Decrypt(message, crypto.Auto)
	if err != nil {
		return nil, nil, err
	}
	data := res.Bytes()
	// gopenpgp's MaxDecompressedMessageSize bounds compressed packets only;
	// uncompressed RFC 9580/9980 literals must obey the caller's bound too.
	if int64(len(data)) > maxBytes {
		clear(data)
		return nil, nil, errors.New("decrypt: plaintext exceeds size limit")
	}
	if res.Metadata().IsUtf8() {
		// openpgp.js turns a text literal's canonical CRLF back into LF.
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	return data, signatures(&res.VerifyResult, keys), nil
}

// reencrypt decrypts the outer OpenPGP session layer of an armored PGP MESSAGE
// using decryptionKeys and wraps the exact inner OpenPGP packet stream (literal
// data, inline signatures, compression, or MIME) inside a fresh v6 ML-KEM-768+
// X25519 PKESK + SEIPDv2 AEAD envelope with a newly generated 256-bit AES-OCB
// session key. Preserving the inner packet stream byte-for-byte ensures every
// inner OpenPGP signature (both inline and RFC 3156 multipart/signed) survives.
func reencrypt(message []byte, decryptionKeys [][]byte, encryptionKey []byte, maxBytes int64) (string, error) {
	if maxBytes < 1 {
		return "", errors.New("reencrypt: invalid plaintext size limit")
	}
	if len(decryptionKeys) == 0 {
		return "", errors.New("reencrypt: no decryption keys")
	}
	if len(encryptionKey) == 0 {
		return "", errors.New("reencrypt: no target encryption key")
	}
	decRing, _, err := loadKeys(decryptionKeys, true, false)
	if err != nil {
		return "", err
	}
	defer decRing.ClearPrivateParams()

	targetRing, targetVersion, err := loadKeys([][]byte{encryptionKey}, false, true)
	if err != nil {
		return "", err
	}
	if targetVersion != 6 || targetRing.CountEntities() != 1 {
		return "", errors.New("reencrypt: target key must be a single v6 certificate")
	}
	encSub, ok := targetRing.GetKeys()[0].GetEntity().EncryptionKey(time.Now(), nil)
	if !ok || encSub.PublicKey == nil || encSub.PublicKey.Version != 6 ||
		encSub.PublicKey.PubKeyAlgo != packet.PubKeyAlgoMlkem768X25519 || len(encSub.PublicKey.Fingerprint) != 32 {
		return "", errors.New("reencrypt: target v6 certificate lacks an active ML-KEM-768+X25519 encryption subkey")
	}

	trimmed := strings.TrimSpace(string(message))
	if !strings.HasPrefix(trimmed, "-----BEGIN PGP MESSAGE-----") ||
		!strings.HasSuffix(trimmed, "-----END PGP MESSAGE-----") ||
		strings.Count(trimmed, "-----BEGIN PGP ") != 1 ||
		strings.Count(trimmed, "-----END PGP ") != 1 {
		return "", errors.New("reencrypt: expected a single ASCII-armored PGP MESSAGE")
	}
	block, err := openpgparmor.Decode(strings.NewReader(trimmed))
	if err != nil {
		return "", fmt.Errorf("reencrypt: decode armor: %w", err)
	}
	if block.Type != "PGP MESSAGE" {
		return "", fmt.Errorf("reencrypt: unexpected armor type %q", block.Type)
	}

	pr := packet.NewReader(block.Body)
	var pkesks []*packet.EncryptedKey
	var se *packet.SymmetricallyEncrypted
	for {
		p, err := pr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reencrypt: read packet: %w", err)
		}
		if se != nil {
			return "", errors.New("reencrypt: unexpected packet after encrypted data")
		}
		switch pkt := p.(type) {
		case *packet.EncryptedKey:
			if len(pkesks) >= 64 {
				return "", errors.New("reencrypt: too many PKESK packets")
			}
			pkesks = append(pkesks, pkt)
		case *packet.SymmetricallyEncrypted:
			if len(pkesks) == 0 {
				return "", errors.New("reencrypt: encrypted message has no PKESK packet")
			}
			if !pkt.IntegrityProtected {
				return "", errors.New("reencrypt: refusing unauthenticated legacy encrypted data")
			}
			se = pkt
			goto decryptSEIPD
		default:
			return "", fmt.Errorf("reencrypt: unexpected outer packet %T", p)
		}
	}
	return "", errors.New("reencrypt: incomplete encrypted message")

decryptSEIPD:
	sessionKey, cipherFunc, err := decryptAnyPKESK(pkesks, se, decRing)
	if err != nil {
		return "", err
	}
	defer clear(sessionKey)

	plainReader, err := se.Decrypt(cipherFunc, sessionKey)
	if err != nil {
		return "", fmt.Errorf("reencrypt: decrypt SEIPD: %w", err)
	}
	innerBytes, readErr := io.ReadAll(io.LimitReader(plainReader, maxBytes+1))
	defer clear(innerBytes)
	closeErr := plainReader.Close()
	if readErr != nil {
		return "", fmt.Errorf("reencrypt: read inner packets: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("reencrypt: authenticate inner stream: %w", closeErr)
	}
	if int64(len(innerBytes)) > maxBytes {
		return "", errors.New("reencrypt: plaintext exceeds size limit")
	}
	if _, err := pr.Next(); !errors.Is(err, io.EOF) {
		return "", errors.New("reencrypt: unexpected trailing outer packet")
	}
	if _, err := packet.NewReader(bytes.NewReader(innerBytes)).Next(); err != nil {
		return "", fmt.Errorf("reencrypt: decrypted inner stream is not valid OpenPGP: %w", err)
	}

	newSessionKey := make([]byte, 32)
	defer clear(newSessionKey)
	if _, err := rand.Read(newSessionKey); err != nil {
		return "", fmt.Errorf("reencrypt: generate session key: %w", err)
	}

	var out bytes.Buffer
	cfg := &packet.Config{
		DefaultCipher: packet.CipherAES256,
		AEADConfig:    &packet.AEADConfig{DefaultMode: packet.AEADModeOCB},
	}
	if err := packet.SerializeEncryptedKeyAEAD(&out, encSub.PublicKey, packet.CipherAES256, true, newSessionKey, cfg); err != nil {
		return "", fmt.Errorf("reencrypt: serialize v6 PKESK: %w", err)
	}
	seWriter, err := packet.SerializeSymmetricallyEncrypted(
		&out,
		packet.CipherAES256,
		true,
		packet.CipherSuite{Cipher: packet.CipherAES256, Mode: packet.AEADModeOCB},
		newSessionKey,
		cfg,
	)
	if err != nil {
		return "", fmt.Errorf("reencrypt: serialize SEIPDv2: %w", err)
	}
	if _, err := seWriter.Write(innerBytes); err != nil {
		_ = seWriter.Close()
		return "", fmt.Errorf("reencrypt: write SEIPDv2 body: %w", err)
	}
	if err := seWriter.Close(); err != nil {
		return "", fmt.Errorf("reencrypt: finalize SEIPDv2: %w", err)
	}
	return armor.ArmorWithType(out.Bytes(), "PGP MESSAGE")
}

func decryptAnyPKESK(pkesks []*packet.EncryptedKey, se *packet.SymmetricallyEncrypted, decRing *crypto.KeyRing) ([]byte, packet.CipherFunction, error) {
	var privKeys []*packet.PrivateKey
	for _, key := range decRing.GetKeys() {
		entity := key.GetEntity()
		if entity == nil {
			continue
		}
		if entity.PrivateKey != nil && !entity.PrivateKey.Encrypted {
			privKeys = append(privKeys, entity.PrivateKey)
		}
		for _, sk := range entity.Subkeys {
			if sk.PrivateKey != nil && !sk.PrivateKey.Encrypted {
				privKeys = append(privKeys, sk.PrivateKey)
			}
		}
	}
	for _, ek := range pkesks {
		for _, priv := range privKeys {
			if !pkeskMatchesPrivateKey(ek, priv) {
				continue
			}
			if err := ek.Decrypt(priv, nil); err == nil && len(ek.Key) > 0 {
				cipher := ek.CipherFunc
				if ek.Version == 6 {
					cipher = se.Cipher
				}
				sk := append([]byte(nil), ek.Key...)
				clear(ek.Key)
				return sk, cipher, nil
			}
		}
	}
	return nil, 0, errors.New("reencrypt: no decryption key matched the message")
}

func pkeskMatchesPrivateKey(ek *packet.EncryptedKey, priv *packet.PrivateKey) bool {
	if ek.Version == 6 {
		return len(ek.KeyFingerprint) == 0 || bytes.Equal(ek.KeyFingerprint, priv.PublicKey.Fingerprint)
	}
	return ek.KeyId == 0 || ek.KeyId == priv.PublicKey.KeyId
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
	return signatures(res, nil), nil
}

// splitPackets cuts go-crypto's own serialization into whole packets. It
// writes only new-format headers with definite lengths (RFC 9580 4.2.1).
func splitPackets(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if b[0]&0xC0 != 0xC0 || len(b) < 2 {
			return nil, errors.New("unexpected packet header")
		}
		var hdr, n int
		switch l := int(b[1]); {
		case l < 192:
			hdr, n = 2, l
		case l < 224 && len(b) >= 3:
			hdr, n = 3, (l-192)<<8+int(b[2])+192
		case l == 255 && len(b) >= 6:
			hdr, n = 6, int(b[2])<<24|int(b[3])<<16|int(b[4])<<8|int(b[5])
		default:
			return nil, errors.New("unexpected packet length")
		}
		if n < 0 || hdr+n > len(b) {
			return nil, errors.New("truncated packet")
		}
		out = append(out, b[:hdr+n])
		b = b[hdr+n:]
	}
	return out, nil
}

// bytesOrUnarmored returns binary OpenPGP data, dearmoring the first block.
func bytesOrUnarmored(data []byte) []byte {
	if raw, err := armor.UnarmorBytes(data); err == nil {
		return raw
	}
	return data
}

func armorBytes(data []byte, armorType string) (string, error) {
	return armor.ArmorWithType(data, armorType)
}

func dearmor(data []byte) ([]byte, error) {
	return armor.UnarmorBytes(data)
}

// Read only the unencrypted envelope. Every session-key recipient must use the
// hybrid KEM; one classical PKESK makes the copy classical for everyone.
func quantumSafeMessage(message []byte) bool {
	reader := bytes.NewReader(bytesOrUnarmored(message))
	count, version := 0, 0
	for {
		p, err := packet.Read(reader)
		if err != nil {
			return false
		}
		switch p := p.(type) {
		case *packet.EncryptedKey:
			if p.Algo != packet.PubKeyAlgoMlkem768X25519 || (version != 0 && version != p.Version) {
				return false
			}
			count++
			version = p.Version
		case *packet.SymmetricallyEncrypted:
			return count > 0 && ((version == 6 && p.Version == 2) || (version == 3 && p.Version == 1))
		default:
			return false
		}
	}
}
