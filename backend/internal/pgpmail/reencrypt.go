package pgpmail

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

const maxInspectedPKESKs = 64

// PKESKInfo summarizes one OpenPGP Public-Key Encrypted Session Key packet
// without decrypting anything.
type PKESKInfo struct {
	Version     int
	KeyID       uint64
	KeyVersion  int
	Fingerprint string // uppercase hex, present on v6 PKESKs (and empty if anonymous)
	Algo        packet.PublicKeyAlgorithm
}

// EncryptedPacketSummary describes the unencrypted packet framing of an
// armored PGP MESSAGE block.
type EncryptedPacketSummary struct {
	PKESKs                  []PKESKInfo
	SEIPDVersion            int
	SEIPDIntegrityProtected bool
	AEADMode                packet.AEADMode
	Cipher                  packet.CipherFunction
}

// AccountArchiveKeys holds the server-side public key identifiers used to
// classify stored messages for P6 archive re-encryption and validate v6
// replacements without ever holding or touching private key material.
type AccountArchiveKeys struct {
	// V4KeyIDs contains the 64-bit OpenPGP key IDs of the account's current v4
	// certificate (primary + subkeys) and any historical 40-hex v4 fingerprints
	// recorded in the account's keyring metadata.
	V4KeyIDs map[uint64]bool
	// V6EncSubkeyFingerprints contains the uppercase 64-hex fingerprint(s) of
	// the account's active v6 ML-KEM-768+X25519 encryption subkey.
	V6EncSubkeyFingerprints map[string]bool
	// V6AllFingerprints contains uppercase 64-hex fingerprints of all keys
	// (primary + subkeys) on the account's v6 certificate.
	V6AllFingerprints map[string]bool
	// V6PrimaryFingerprint is the uppercase 64-hex primary fingerprint of the
	// account's v6 certificate.
	V6PrimaryFingerprint string
}

// ArchiveKeysForAccount derives the public key IDs and v6 encryption subkey
// fingerprint for archive re-encryption from the user's stored public keys.
func ArchiveKeysForAccount(v4PublicKey, v6PublicKey string, ringKeyFingerprints []string) (AccountArchiveKeys, error) {
	if strings.TrimSpace(v6PublicKey) == "" {
		return AccountArchiveKeys{}, errors.New("account has no v6 public certificate")
	}
	v6Info, err := InspectPQCKey(v6PublicKey)
	if err != nil {
		return AccountArchiveKeys{}, fmt.Errorf("invalid v6 public certificate: %w", err)
	}
	v6Key, err := crypto.NewKeyFromArmored(v6PublicKey)
	if err != nil {
		return AccountArchiveKeys{}, fmt.Errorf("parse v6 public certificate: %w", err)
	}
	e6 := v6Key.GetEntity()
	if e6 == nil || e6.PrimaryKey == nil || len(e6.Subkeys) == 0 {
		return AccountArchiveKeys{}, errors.New("v6 public certificate has no encryption subkey")
	}

	out := AccountArchiveKeys{
		V4KeyIDs:                make(map[uint64]bool),
		V6EncSubkeyFingerprints: make(map[string]bool),
		V6AllFingerprints:       make(map[string]bool),
		V6PrimaryFingerprint:    strings.ToUpper(v6Info.Fingerprint),
	}
	out.V6AllFingerprints[strings.ToUpper(hex.EncodeToString(e6.PrimaryKey.Fingerprint))] = true
	for _, sk := range e6.Subkeys {
		if sk.PublicKey == nil {
			continue
		}
		fp := strings.ToUpper(hex.EncodeToString(sk.PublicKey.Fingerprint))
		out.V6AllFingerprints[fp] = true
		if sk.PublicKey.Version == 6 && sk.PublicKey.PubKeyAlgo == packet.PubKeyAlgoMlkem768X25519 && len(sk.PublicKey.Fingerprint) == 32 {
			out.V6EncSubkeyFingerprints[fp] = true
		}
	}
	if len(out.V6EncSubkeyFingerprints) == 0 {
		return AccountArchiveKeys{}, errors.New("v6 public certificate has no ML-KEM-768+X25519 encryption subkey")
	}

	if strings.TrimSpace(v4PublicKey) != "" {
		if v4Key, err := crypto.NewKeyFromArmored(v4PublicKey); err == nil {
			if e4 := v4Key.GetEntity(); e4 != nil {
				if e4.PrimaryKey != nil && e4.PrimaryKey.KeyId != 0 {
					out.V4KeyIDs[e4.PrimaryKey.KeyId] = true
				}
				for _, sk := range e4.Subkeys {
					if sk.PublicKey != nil && sk.PublicKey.KeyId != 0 {
						out.V4KeyIDs[sk.PublicKey.KeyId] = true
					}
				}
			}
		}
	}

	for _, rawFP := range ringKeyFingerprints {
		fp := strings.TrimSpace(rawFP)
		// OpenPGP v4 fingerprints are 40 hex chars (20 bytes); the 64-bit KeyID
		// is the trailing 16 hex chars (8 bytes).
		if len(fp) != 40 {
			continue
		}
		if id, err := strconv.ParseUint(fp[24:], 16, 64); err == nil && id != 0 {
			out.V4KeyIDs[id] = true
		}
	}

	return out, nil
}

// ExtractPGPMIMEEncryptedPayload extracts the ASCII-armored OpenPGP message
// from a strict RFC 3156 multipart/encrypted RFC 5322 message. It returns an
// error for plaintext, multipart/signed, malformed envelopes, or envelopes
// with unexpected parts.
func ExtractPGPMIMEEncryptedPayload(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("empty message")
	}
	if int64(len(raw)) > mailmsg.MaxInboundMessageBytes {
		return "", mailmsg.ErrMessageTooLarge
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse RFC 5322 headers: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "multipart/encrypted") {
		return "", errors.New("message is not multipart/encrypted")
	}
	if !strings.EqualFold(strings.TrimSpace(params["protocol"]), "application/pgp-encrypted") {
		return "", errors.New("multipart/encrypted protocol is not application/pgp-encrypted")
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", errors.New("multipart/encrypted missing boundary")
	}

	mr := multipart.NewReader(msg.Body, boundary)

	// Part 1: application/pgp-encrypted control part ("Version: 1").
	part1, err := mr.NextPart()
	if err != nil {
		return "", fmt.Errorf("read PGP/MIME control part: %w", err)
	}
	p1Type, _, err := mime.ParseMediaType(part1.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(p1Type, "application/pgp-encrypted") {
		_ = part1.Close()
		return "", errors.New("PGP/MIME part 1 is not application/pgp-encrypted")
	}
	p1Body, err := readDecodedPart(part1, 4096)
	_ = part1.Close()
	if err != nil {
		return "", fmt.Errorf("read PGP/MIME control part body: %w", err)
	}
	if !hasPGPMIMEVersion1(p1Body) {
		return "", errors.New("PGP/MIME control part missing Version: 1")
	}

	// Part 2: application/octet-stream carrying the armored PGP MESSAGE.
	part2, err := mr.NextPart()
	if err != nil {
		return "", fmt.Errorf("read PGP/MIME payload part: %w", err)
	}
	p2Type, _, err := mime.ParseMediaType(part2.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(p2Type, "application/octet-stream") {
		_ = part2.Close()
		return "", errors.New("PGP/MIME part 2 is not application/octet-stream")
	}
	p2Body, err := readDecodedPart(part2, mailmsg.MaxInboundMessageBytes)
	_ = part2.Close()
	if err != nil {
		return "", fmt.Errorf("read PGP/MIME payload part body: %w", err)
	}

	// RFC 3156 §4 forbids more than two parts inside multipart/encrypted.
	if extra, err := mr.NextPart(); err != io.EOF {
		if extra != nil {
			_ = extra.Close()
		}
		return "", errors.New("multipart/encrypted contains unexpected extra part")
	}

	armored := strings.TrimSpace(string(p2Body))
	if !isSingleArmoredPGPMessage(armored) {
		return "", errors.New("PGP/MIME payload is not a single armored PGP MESSAGE")
	}
	return armored, nil
}

func readDecodedPart(part *multipart.Part, maxBytes int64) ([]byte, error) {
	var r io.Reader = part
	switch strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit", "binary":
	case "quoted-printable":
		r = quotedprintable.NewReader(part)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, part)
	default:
		return nil, errors.New("unsupported content-transfer-encoding")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, mailmsg.ErrMessageTooLarge
	}
	return data, nil
}

func hasPGPMIMEVersion1(body []byte) bool {
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "Version") && strings.TrimSpace(v) == "1" {
			return true
		}
	}
	return false
}

func isSingleArmoredPGPMessage(trimmed string) bool {
	return strings.HasPrefix(trimmed, "-----BEGIN PGP MESSAGE-----") &&
		strings.HasSuffix(trimmed, "-----END PGP MESSAGE-----") &&
		strings.Count(trimmed, "-----BEGIN PGP ") == 1 &&
		strings.Count(trimmed, "-----END PGP ") == 1
}

// InspectEncryptedPackets parses the outer OpenPGP packet sequence of an
// ASCII-armored PGP MESSAGE without decrypting anything. It requires at least
// one PKESK packet followed by exactly one SymmetricallyEncrypted (SEIPD)
// packet and nothing else.
func InspectEncryptedPackets(armored string) (EncryptedPacketSummary, error) {
	trimmed := strings.TrimSpace(armored)
	if !isSingleArmoredPGPMessage(trimmed) {
		return EncryptedPacketSummary{}, errors.New("expected a single ASCII-armored PGP MESSAGE block")
	}
	block, err := armor.Decode(strings.NewReader(trimmed))
	if err != nil {
		return EncryptedPacketSummary{}, fmt.Errorf("decode OpenPGP armor: %w", err)
	}
	if block.Type != "PGP MESSAGE" {
		return EncryptedPacketSummary{}, fmt.Errorf("unexpected armor type %q", block.Type)
	}

	pr := packet.NewReader(block.Body)
	var summary EncryptedPacketSummary
	sawSEIPD := false

	for {
		p, err := pr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return EncryptedPacketSummary{}, fmt.Errorf("parse OpenPGP packet: %w", err)
		}
		if sawSEIPD {
			return EncryptedPacketSummary{}, errors.New("unexpected packet after symmetrically encrypted data")
		}
		switch pkt := p.(type) {
		case *packet.EncryptedKey:
			if len(summary.PKESKs) >= maxInspectedPKESKs {
				return EncryptedPacketSummary{}, errors.New("too many PKESK packets")
			}
			summary.PKESKs = append(summary.PKESKs, PKESKInfo{
				Version:     pkt.Version,
				KeyID:       pkt.KeyId,
				KeyVersion:  pkt.KeyVersion,
				Fingerprint: strings.ToUpper(hex.EncodeToString(pkt.KeyFingerprint)),
				Algo:        pkt.Algo,
			})
		case *packet.SymmetricallyEncrypted:
			if len(summary.PKESKs) == 0 {
				return EncryptedPacketSummary{}, errors.New("encrypted message has no PKESK packet")
			}
			sawSEIPD = true
			summary.SEIPDVersion = pkt.Version
			summary.SEIPDIntegrityProtected = pkt.IntegrityProtected
			summary.AEADMode = pkt.Mode
			summary.Cipher = pkt.Cipher
			// Drain the encrypted packet body so packet.Reader can verify
			// there are no trailing outer packets.
			if _, err := io.Copy(io.Discard, pkt.Contents); err != nil {
				return EncryptedPacketSummary{}, fmt.Errorf("read SEIPD body: %w", err)
			}
		default:
			return EncryptedPacketSummary{}, fmt.Errorf("unexpected outer OpenPGP packet %T", p)
		}
	}

	if len(summary.PKESKs) == 0 || !sawSEIPD {
		return EncryptedPacketSummary{}, errors.New("incomplete OpenPGP encrypted message")
	}
	return summary, nil
}

// IsV4ArchiveCandidate reports whether raw is a PGP/MIME encrypted message
// whose PKESKs address the account's v4 key(s) and NOT the account's v6 key.
// Plaintext messages, malformed messages, and messages already encrypted to
// the account's v6 certificate return false.
func IsV4ArchiveCandidate(raw []byte, keys AccountArchiveKeys) bool {
	if len(keys.V4KeyIDs) == 0 {
		return false
	}
	armored, err := ExtractPGPMIMEEncryptedPayload(raw)
	if err != nil {
		return false
	}
	summary, err := InspectEncryptedPackets(armored)
	if err != nil {
		return false
	}
	hasAccountV4 := false
	for _, p := range summary.PKESKs {
		if p.Version == 6 && (keys.V6EncSubkeyFingerprints[p.Fingerprint] || keys.V6AllFingerprints[p.Fingerprint]) {
			return false
		}
		if p.Version == 3 && p.KeyID != 0 && keys.V4KeyIDs[p.KeyID] {
			hasAccountV4 = true
		}
	}
	return hasAccountV4
}

// ValidateV6ArchiveCiphertext enforces the P6 server acceptance invariant: the
// replacement ciphertext must be a single ASCII-armored PGP MESSAGE whose only
// PKESK is a v6 ML-KEM-768+X25519 packet naming the account's current v6
// encryption subkey, followed by a SEIPDv2 AEAD packet and nothing else.
func ValidateV6ArchiveCiphertext(armored string, keys AccountArchiveKeys) error {
	if len(keys.V6EncSubkeyFingerprints) == 0 {
		return errors.New("account has no active v6 encryption subkey")
	}
	summary, err := InspectEncryptedPackets(armored)
	if err != nil {
		return err
	}
	if len(summary.PKESKs) != 1 {
		return fmt.Errorf("replacement ciphertext must contain exactly 1 PKESK, got %d", len(summary.PKESKs))
	}
	p := summary.PKESKs[0]
	if p.Version != 6 || p.KeyVersion != 6 {
		return fmt.Errorf("replacement PKESK must be v6, got v%d (key v%d)", p.Version, p.KeyVersion)
	}
	if p.Algo != packet.PubKeyAlgoMlkem768X25519 {
		return fmt.Errorf("replacement PKESK must use ML-KEM-768+X25519, got algo %d", p.Algo)
	}
	if len(p.Fingerprint) != 64 || !keys.V6EncSubkeyFingerprints[p.Fingerprint] {
		return errors.New("replacement PKESK does not name the account's current v6 encryption subkey")
	}
	if summary.SEIPDVersion != 2 || !summary.SEIPDIntegrityProtected || summary.Cipher == 0 || summary.AEADMode == 0 {
		return fmt.Errorf("replacement ciphertext must use SEIPDv2 AEAD, got version %d", summary.SEIPDVersion)
	}
	return nil
}

// RebuildPGPMIMEMessage replaces only the encrypted MIME payload of oldRaw
// with newArmored inside a fresh RFC 3156 multipart/encrypted envelope while
// preserving all non-MIME outer RFC 5322 headers verbatim (including Received,
// Authentication-Results, From, To, Cc, Subject, Date, Message-ID, References,
// In-Reply-To, and custom headers).
func RebuildPGPMIMEMessage(oldRaw []byte, newArmored string) ([]byte, error) {
	trimmed := strings.TrimSpace(newArmored)
	if !isSingleArmoredPGPMessage(trimmed) {
		return nil, errors.New("replacement payload is not a single armored PGP MESSAGE")
	}
	headerBlock, err := extractRawHeaderBlock(oldRaw)
	if err != nil {
		return nil, err
	}
	preservedHeaders := filterPreservedOuterHeaders(headerBlock)

	var out bytes.Buffer
	out.Write(preservedHeaders)
	out.WriteString("MIME-Version: 1.0\r\n")

	boundary := randomBoundary()
	w := multipart.NewWriter(&out)
	if err := w.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set MIME boundary: %w", err)
	}
	out.WriteString(`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary="` + boundary + `"` + "\r\n")
	out.WriteString("\r\n")

	control, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/pgp-encrypted"}})
	if err != nil {
		return nil, fmt.Errorf("write control part: %w", err)
	}
	if _, err := io.WriteString(control, "Version: 1\r\n"); err != nil {
		return nil, fmt.Errorf("write control part body: %w", err)
	}

	data, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {`application/octet-stream; name="encrypted.asc"`},
		"Content-Disposition": {`inline; filename="encrypted.asc"`},
	})
	if err != nil {
		return nil, fmt.Errorf("write payload part: %w", err)
	}
	canonicalArmor := strings.ReplaceAll(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	if _, err := io.WriteString(data, canonicalArmor); err != nil {
		return nil, fmt.Errorf("write payload part body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	result := out.Bytes()
	if int64(len(result)) > mailmsg.MaxInboundMessageBytes {
		return nil, mailmsg.ErrMessageTooLarge
	}
	return result, nil
}

func extractRawHeaderBlock(raw []byte) ([]byte, error) {
	if idx := bytes.Index(raw, []byte("\r\n\r\n")); idx >= 0 {
		return raw[:idx], nil
	}
	if idx := bytes.Index(raw, []byte("\n\n")); idx >= 0 {
		return raw[:idx], nil
	}
	return nil, errors.New("message missing RFC 5322 header separator")
}

// filterPreservedOuterHeaders keeps every outer RFC 5322 header (including
// multi-line folded continuations) in original order with CRLF endings,
// stripping only the root MIME framing headers that RebuildPGPMIMEMessage
// regenerates.
func filterPreservedOuterHeaders(headerBlock []byte) []byte {
	normalized := strings.ReplaceAll(string(headerBlock), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var out bytes.Buffer
	skip := false
	for _, line := range lines {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !skip {
				out.WriteString(line)
				out.WriteString("\r\n")
			}
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			skip = true
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-type", "content-transfer-encoding", "content-disposition", "mime-version":
			skip = true
		default:
			skip = false
			out.WriteString(line)
			out.WriteString("\r\n")
		}
	}
	return out.Bytes()
}
