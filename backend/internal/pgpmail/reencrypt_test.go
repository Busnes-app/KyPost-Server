package pgpmail

import (
	"bytes"
	"crypto/rand"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpprofile"
)

func generateTestV4Key(t *testing.T, name, email string) *crypto.Key {
	t.Helper()
	key, err := crypto.PGP().KeyGeneration().
		AddUserId(name, email).New().GenerateKey()
	if err != nil {
		t.Fatalf("generate v4 key: %v", err)
	}
	return key
}

func generateTestV6Key(t *testing.T, name, email string) *crypto.Key {
	t.Helper()
	key, err := pgpprofile.V6().KeyGeneration().
		AddUserId(name, email).New().GenerateKey()
	if err != nil {
		t.Fatalf("generate v6 key: %v", err)
	}
	return key
}

func armorPub(t *testing.T, k *crypto.Key) string {
	t.Helper()
	pub, err := k.GetArmoredPublicKey()
	if err != nil {
		t.Fatalf("armor public key: %v", err)
	}
	return pub
}

func encryptToKeys(t *testing.T, plaintext []byte, recipients ...*crypto.Key) string {
	t.Helper()
	ring, err := crypto.NewKeyRing(recipients[0])
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	for _, k := range recipients[1:] {
		if err := ring.AddKey(k); err != nil {
			t.Fatalf("add recipient: %v", err)
		}
	}
	handle := crypto.PGP()
	if recipients[0].GetVersion() == 6 {
		handle = pgpprofile.V6()
	}
	h, err := handle.Encryption().Recipients(ring).New()
	if err != nil {
		t.Fatalf("build encryption handle: %v", err)
	}
	msg, err := h.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	armored, err := msg.Armor()
	if err != nil {
		t.Fatalf("armor: %v", err)
	}
	return armored
}

func TestIsV4ArchiveCandidate(t *testing.T) {
	acctV4 := generateTestV4Key(t, "Alice", "alice@example.test")
	acctV4Old := generateTestV4Key(t, "Alice Old", "alice@example.test")
	acctV6 := generateTestV6Key(t, "Alice", "alice@example.test")
	otherV4 := generateTestV4Key(t, "Bob", "bob@example.test")

	// Collect historical v4 subkey fingerprint (40 hex) as stored in keyring metadata.
	var oldRingFPs []string
	oldRingFPs = append(oldRingFPs, strings.ToUpper(acctV4Old.GetFingerprint()))
	for _, sk := range acctV4Old.GetEntity().Subkeys {
		oldRingFPs = append(oldRingFPs, strings.ToUpper(bytesToHex(sk.PublicKey.Fingerprint)))
	}

	keys, err := ArchiveKeysForAccount(armorPub(t, acctV4), armorPub(t, acctV6), oldRingFPs)
	if err != nil {
		t.Fatalf("ArchiveKeysForAccount: %v", err)
	}

	plainRFC5322 := []byte(
		"From: bob@example.test\r\n" +
			"To: alice@example.test\r\n" +
			"Subject: Hello\r\n" +
			"Date: Mon, 01 Jan 2026 12:00:00 +0000\r\n" +
			"Message-ID: <m1@example.test>\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
			"Secret body\r\n",
	)

	v4Stored, err := EncryptStoredMIME(plainRFC5322, armorPub(t, acctV4))
	if err != nil {
		t.Fatalf("EncryptStoredMIME v4: %v", err)
	}
	v6Stored, err := EncryptStoredMIME(plainRFC5322, armorPub(t, acctV6))
	if err != nil {
		t.Fatalf("EncryptStoredMIME v6: %v", err)
	}
	oldV4Stored, err := EncryptStoredMIME(plainRFC5322, armorPub(t, acctV4Old))
	if err != nil {
		t.Fatalf("EncryptStoredMIME old v4: %v", err)
	}
	otherV4Stored, err := EncryptStoredMIME(plainRFC5322, armorPub(t, otherV4))
	if err != nil {
		t.Fatalf("EncryptStoredMIME other v4: %v", err)
	}
	multiV4Stored, err := EncryptMIME(plainRFC5322, []string{armorPub(t, acctV4), armorPub(t, otherV4)}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME multi v4: %v", err)
	}

	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"v4 incoming encrypted", v4Stored, true},
		{"historical retired v4 key in ring", oldV4Stored, true},
		{"outside multi-recipient v4 including account", multiV4Stored, true},
		{"already v6 encrypted", v6Stored, false},
		{"plaintext message", plainRFC5322, false},
		{"v4 encrypted only to another user", otherV4Stored, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsV4ArchiveCandidate(tc.raw, keys); got != tc.want {
				t.Fatalf("IsV4ArchiveCandidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateV6ArchiveCiphertext(t *testing.T) {
	acctV4 := generateTestV4Key(t, "Alice", "alice@example.test")
	acctV6 := generateTestV6Key(t, "Alice", "alice@example.test")
	otherV6 := generateTestV6Key(t, "Bob", "bob@example.test")

	keys, err := ArchiveKeysForAccount(armorPub(t, acctV4), armorPub(t, acctV6), nil)
	if err != nil {
		t.Fatalf("ArchiveKeysForAccount: %v", err)
	}

	validV6 := encryptToKeys(t, []byte("hello"), acctV6)
	if err := ValidateV6ArchiveCiphertext(validV6, keys); err != nil {
		t.Fatalf("valid v6 ciphertext rejected: %v", err)
	}

	// Refuse v4 ciphertext (v3 PKESK + SEIPDv1).
	v4Cipher := encryptToKeys(t, []byte("hello"), acctV4)
	if err := ValidateV6ArchiveCiphertext(v4Cipher, keys); err == nil {
		t.Fatal("expected v4 ciphertext to be rejected")
	}

	// Refuse v6 ciphertext encrypted to a different user's v6 key.
	otherV6Cipher := encryptToKeys(t, []byte("hello"), otherV6)
	if err := ValidateV6ArchiveCiphertext(otherV6Cipher, keys); err == nil {
		t.Fatal("expected other user's v6 ciphertext to be rejected")
	}

	// Refuse v6 ciphertext encrypted to BOTH account v6 AND another user's v6 key.
	multiV6Cipher := encryptToKeys(t, []byte("hello"), acctV6, otherV6)
	if err := ValidateV6ArchiveCiphertext(multiV6Cipher, keys); err == nil {
		t.Fatal("expected multi-recipient v6 ciphertext to be rejected")
	}

	// Refuse anonymous v6 PKESK (zero/empty fingerprint).
	anonV6 := makeAnonymousV6Ciphertext(t, acctV6)
	if err := ValidateV6ArchiveCiphertext(anonV6, keys); err == nil {
		t.Fatal("expected anonymous v6 PKESK to be rejected")
	}

	// Refuse plaintext literal data disguised as PGP MESSAGE.
	if err := ValidateV6ArchiveCiphertext("not a pgp message", keys); err == nil {
		t.Fatal("expected plaintext to be rejected")
	}

	// Refuse symmetric password-encrypted message (SKESK, no PKESK).
	symHandle, err := pgpprofile.V6().Encryption().Password([]byte("secret")).New()
	if err != nil {
		t.Fatalf("sym handle: %v", err)
	}
	symMsg, err := symHandle.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("sym encrypt: %v", err)
	}
	symArmored, err := symMsg.Armor()
	if err != nil {
		t.Fatalf("sym armor: %v", err)
	}
	if err := ValidateV6ArchiveCiphertext(symArmored, keys); err == nil {
		t.Fatal("expected SKESK ciphertext to be rejected")
	}

	// Refuse second armor block appended.
	if err := ValidateV6ArchiveCiphertext(validV6+"\n"+validV6, keys); err == nil {
		t.Fatal("expected multiple armor blocks to be rejected")
	}
}

func makeAnonymousV6Ciphertext(t *testing.T, recipient *crypto.Key) string {
	t.Helper()
	encSub, ok := recipient.GetEntity().EncryptionKey(time.Now(), nil)
	if !ok {
		t.Fatal("no encryption subkey")
	}
	// Clone public key with empty fingerprint to force anonymous PKESK.
	pubCopy := *encSub.PublicKey
	pubCopy.Fingerprint = nil
	pubCopy.KeyId = 0

	var raw bytes.Buffer
	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	cfg := &packet.Config{
		DefaultCipher: packet.CipherAES256,
		AEADConfig:    &packet.AEADConfig{DefaultMode: packet.AEADModeOCB},
	}
	if err := packet.SerializeEncryptedKeyAEAD(&raw, &pubCopy, packet.CipherAES256, true, sk, cfg); err != nil {
		t.Fatalf("SerializeEncryptedKeyAEAD: %v", err)
	}
	w, err := packet.SerializeSymmetricallyEncrypted(&raw, packet.CipherAES256, true, packet.CipherSuite{Cipher: packet.CipherAES256, Mode: packet.AEADModeOCB}, sk, cfg)
	if err != nil {
		t.Fatalf("SerializeSymmetricallyEncrypted: %v", err)
	}
	_, _ = w.Write([]byte("payload"))
	_ = w.Close()

	var arm bytes.Buffer
	aw, err := armor.Encode(&arm, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatalf("armor encode: %v", err)
	}
	_, _ = aw.Write(raw.Bytes())
	_ = aw.Close()
	return arm.String()
}

func TestRebuildPGPMIMEMessagePreservesOuterHeaders(t *testing.T) {
	acctV4 := generateTestV4Key(t, "Alice", "alice@example.test")
	acctV6 := generateTestV6Key(t, "Alice", "alice@example.test")
	keys, err := ArchiveKeysForAccount(armorPub(t, acctV4), armorPub(t, acctV6), nil)
	if err != nil {
		t.Fatalf("ArchiveKeysForAccount: %v", err)
	}

	oldArmored := encryptToKeys(t, []byte("inner"), acctV4)
	oldRaw := []byte(
		"Received: from mx.example.net (mx.example.net [192.0.2.1])\r\n" +
			"\tby mail.example.test with ESMTPS id abc123\r\n" +
			"Authentication-Results: mail.example.test; dkim=pass header.d=example.net\r\n" +
			"From: Bob <bob@example.net>\r\n" +
			"To: Alice <alice@example.test>\r\n" +
			"Subject: [Encrypted] Email Sent by KyPost\r\n" +
			"Date: Tue, 02 Jan 2026 08:30:00 +0000\r\n" +
			"Message-ID: <preserve-me@example.net>\r\n" +
			"In-Reply-To: <parent@example.net>\r\n" +
			"References: <root@example.net> <parent@example.net>\r\n" +
			"X-Custom-Header: keep-verbatim\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=\"old-b\"\r\n" +
			"\r\n" +
			"--old-b\r\n" +
			"Content-Type: application/pgp-encrypted\r\n\r\n" +
			"Version: 1\r\n" +
			"--old-b\r\n" +
			"Content-Type: application/octet-stream\r\n\r\n" +
			oldArmored + "\r\n" +
			"--old-b--\r\n",
	)

	newArmored := encryptToKeys(t, []byte("inner"), acctV6)
	rebuilt, err := RebuildPGPMIMEMessage(oldRaw, newArmored)
	if err != nil {
		t.Fatalf("RebuildPGPMIMEMessage: %v", err)
	}

	parsed, err := mail.ReadMessage(bytes.NewReader(rebuilt))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	if got := parsed.Header.Get("Message-ID"); got != "<preserve-me@example.net>" {
		t.Fatalf("Message-ID = %q", got)
	}
	if got := parsed.Header.Get("In-Reply-To"); got != "<parent@example.net>" {
		t.Fatalf("In-Reply-To = %q", got)
	}
	if got := parsed.Header.Get("X-Custom-Header"); got != "keep-verbatim" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
	if !strings.Contains(parsed.Header.Get("Received"), "by mail.example.test with ESMTPS id abc123") {
		t.Fatalf("folded Received header lost: %q", parsed.Header.Get("Received"))
	}
	if !strings.Contains(parsed.Header.Get("Authentication-Results"), "dkim=pass") {
		t.Fatalf("Authentication-Results lost: %q", parsed.Header.Get("Authentication-Results"))
	}

	extracted, err := ExtractPGPMIMEEncryptedPayload(rebuilt)
	if err != nil {
		t.Fatalf("ExtractPGPMIMEEncryptedPayload(rebuilt): %v", err)
	}
	if err := ValidateV6ArchiveCiphertext(extracted, keys); err != nil {
		t.Fatalf("ValidateV6ArchiveCiphertext(rebuilt): %v", err)
	}
	if IsV4ArchiveCandidate(rebuilt, keys) {
		t.Fatal("rebuilt v6 message must no longer be a v4 candidate")
	}
}

func bytesToHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}
