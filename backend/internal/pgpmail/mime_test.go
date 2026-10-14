package pgpmail_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpautocrypt"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// extractOctetStreamPart is a test-only MIME walker that finds the armored
// PGP data part EncryptMIME produces, mirroring the content-sniffing
// technique (crypto.IsPGPMessage) the receive-path integration (Task 7) uses
// against real IMAP-fetched attachments.
func extractOctetStreamPart(t *testing.T, raw []byte) (string, bool) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := append([]byte("Content-Type: "+msg.Header.Get("Content-Type")+"\r\n\r\n"), body...)
	_, _, attachments, err := pgpmailtest.ParseContent(content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	for _, a := range attachments {
		if crypto.IsPGPMessage(string(a.Content)) {
			return string(a.Content), true
		}
	}
	return "", false
}

func TestEncryptDecryptMIMERoundTrip(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	bob, err := pgpmailtest.GenerateIdentity("Bob", "bob@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity bob: %v", err)
	}

	wantSubject := strings.TrimSpace(strings.Repeat("Secret 日本語 ✉ ", 40))
	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: wantSubject,
		Body:    "meet at dawn",
		Mode:    "plain",
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{bob.ArmoredPublicKey}, alice)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	if !strings.Contains(string(encrypted), "multipart/encrypted") {
		t.Fatal("expected multipart/encrypted content type in output")
	}
	// The real subject must NOT appear in cleartext anywhere on the wire; the
	// outer Subject is replaced with the fixed placeholder.
	if strings.Contains(string(encrypted), "Secret") || strings.Contains(string(encrypted), "=?utf-8?") {
		t.Fatal("real subject leaked into the encrypted message bytes")
	}
	if !strings.Contains(string(encrypted), "Subject: "+pgpmail.OuterPlaceholderSubject) {
		t.Fatal("expected the placeholder Subject on the outer envelope")
	}

	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}

	result, err := pgpmailtest.DecryptMIME(armoredData, bob, []string{alice.ArmoredPublicKey})
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	if !result.Verified {
		t.Fatal("expected signature to verify")
	}
	if result.SignerFingerprint != alice.Fingerprint {
		t.Fatalf("signer fingerprint mismatch: got %s want %s", result.SignerFingerprint, alice.Fingerprint)
	}
	// The real subject is recovered from the decrypted payload's protected
	// headers.
	subject, ok := pgpmailtest.ExtractProtectedSubject(result.Content)
	if !ok || subject != wantSubject {
		t.Fatalf("ExtractProtectedSubject: got (%q, %v), want (%q, true)", subject, ok, wantSubject)
	}
	if _, err := mailmsg.NormalizeSMTPMessage(result.Content); err != nil {
		t.Fatal("protected subject lost folding", err)
	}
	body, _, attachments, err := pgpmailtest.ParseContent(result.Content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if body != "meet at dawn" {
		t.Fatalf("body mismatch: got %q", body)
	}
	// The protected-headers wrapper and its legacy-display part must be
	// transparent to ParseContent — no phantom attachment.
	if len(attachments) != 0 {
		t.Fatalf("expected no attachments, got %d", len(attachments))
	}
}

func TestEncryptDecryptMIMEWithAttachment(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"alice@example.com"},
		Subject: "With attachment",
		Body:    "see attached",
		Mode:    "plain",
		Attachments: []mailmsg.Attachment{
			{Name: "note.txt", MimeType: "text/plain", Content: []byte("hello file")},
		},
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}

	result, err := pgpmailtest.DecryptMIME(armoredData, alice, nil)
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	// EncryptMIME was called with a nil signer above: the resulting
	// ciphertext must not carry an embedded signature. This guards against
	// the encrypt-implicitly-signs regression where a caller's eagerly
	// loaded identity leaked into EncryptMIME's signer argument even when
	// signing wasn't requested.
	if result.Signed {
		t.Fatal("expected unsigned result when EncryptMIME was called with a nil signer")
	}
	body, _, attachments, err := pgpmailtest.ParseContent(result.Content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if body != "see attached" {
		t.Fatalf("body mismatch: got %q", body)
	}
	if len(attachments) != 1 || attachments[0].Name != "note.txt" || string(attachments[0].Content) != "hello file" {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
}

// TestEncryptMIMESanitizesProtectedSubject proves a subject carrying CR/LF
// can't inject headers into the protected wrapper: the injected line never
// appears as its own header.
func TestEncryptMIMESanitizesProtectedSubject(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"alice@example.com"},
		Subject: "safe\r\nBcc: evil@example.com",
		Body:    "hi",
		Mode:    "plain",
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}
	result, err := pgpmailtest.DecryptMIME(armoredData, alice, nil)
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	if bytes.Contains(result.Content, []byte("\r\nBcc:")) {
		t.Fatalf("subject injection: an injected Bcc header appeared in the protected content:\n%s", result.Content)
	}
	// CR and LF are each flattened to a space, so "safe\r\nBcc:..." becomes
	// "safe  Bcc:..." on one line — no injected header.
	subject, ok := pgpmailtest.ExtractProtectedSubject(result.Content)
	if !ok || subject != "safe  Bcc: evil@example.com" {
		t.Fatalf("ExtractProtectedSubject: got (%q, %v), want flattened single line", subject, ok)
	}
}

// TestEncryptMIMENoSubject proves an empty subject still gets the outer
// placeholder path only when there's a subject: with no subject, the message
// is encrypted without a protected-headers wrapper and no placeholder is
// forced.
func TestEncryptMIMENoSubject(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	plaintext := mailmsg.Message{
		From: "alice@example.com",
		To:   []string{"alice@example.com"},
		Body: "hi",
		Mode: "plain",
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	if strings.Contains(string(encrypted), pgpmail.OuterPlaceholderSubject) {
		t.Fatal("did not expect a placeholder Subject when the message had no subject")
	}
	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}
	result, err := pgpmailtest.DecryptMIME(armoredData, alice, nil)
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	if subject, ok := pgpmailtest.ExtractProtectedSubject(result.Content); ok {
		t.Fatalf("expected no protected subject for a subjectless message, got %q", subject)
	}
	body, _, _, err := pgpmailtest.ParseContent(result.Content)
	if err != nil {
		t.Fatalf("ParseContent: %v", err)
	}
	if strings.TrimSpace(body) != "hi" {
		t.Fatalf("body mismatch: got %q", body)
	}
}

// TestEncryptMIMEPreservesAutocryptHeader proves Task 2 of the
// outbound-publishing spec: mailmsg.Message.Autocrypt (Task 1) must survive
// PGP/MIME wrapping on the outer, unencrypted envelope — never inside the
// ciphertext, since Autocrypt exists precisely so a receiving client can
// opportunistically pick up the sender's key without decrypting anything.
func TestEncryptMIMEPreservesAutocryptHeader(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	plaintext := mailmsg.Message{
		From:      "alice@example.com",
		To:        []string{"bob@example.com"},
		Subject:   "hi",
		Body:      "secret",
		Mode:      "plain",
		Autocrypt: "addr=alice@example.com; keydata=QUJD",
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	if !bytes.Contains(encrypted, []byte("\r\nAutocrypt: addr=alice@example.com; keydata=QUJD\r\n")) {
		t.Fatalf("Autocrypt header not on outer envelope:\n%s", encrypted)
	}

	armoredData, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("expected an application/octet-stream data part")
	}
	result, err := pgpmailtest.DecryptMIME(armoredData, alice, nil)
	if err != nil {
		t.Fatalf("DecryptMIME: %v", err)
	}
	if bytes.Contains(result.Content, []byte("Autocrypt")) {
		t.Fatalf("Autocrypt header leaked into the encrypted/decrypted inner content:\n%s", result.Content)
	}
}

// longAutocryptTestKeydata returns base64 keydata roughly the size of an
// imported RSA-3072 key (~2487 octets unfolded), matching the size that
// motivated R1: gopenpgp's default curve25519 keys stay under RFC 5322's
// line-length limit with little headroom, but an imported RSA key does not.
func longAutocryptTestKeydata(t *testing.T) (keyBytes []byte, keydataB64 string) {
	t.Helper()
	keyBytes = make([]byte, 1865)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	keydataB64 = base64.StdEncoding.EncodeToString(keyBytes)
	if len(keydataB64) < 2000 {
		t.Fatalf("test setup bug: keydata too short to exercise folding (%d)", len(keydataB64))
	}
	return keyBytes, keydataB64
}

// assertNoOverlongLines fails the test if any \r\n-delimited line in raw
// exceeds the RFC 5322 §2.1.1 / RFC 5321 §4.5.3.1.6 998/1000-octet limits.
func assertNoOverlongLines(t *testing.T, raw []byte) {
	t.Helper()
	for _, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("line exceeds 998 octets (%d): %q", len(line), line)
		}
	}
}

// TestEncryptMIMEFoldsLongAutocryptHeader covers R1's "critical subtlety":
// splitMessage reads the outer headers via textproto.ReadMIMEHeader, which
// unfolds mailmsg.Build()'s RFC 5322 folding back into one long value, and
// writeEnvelopeHeaders must re-fold the Autocrypt header when re-emitting it
// on the encrypted envelope — otherwise the fold applied on the plain path
// is silently undone on the encrypt path.
func TestEncryptMIMEFoldsLongAutocryptHeader(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity alice: %v", err)
	}
	keyBytes, keydataB64 := longAutocryptTestKeydata(t)
	plaintext := mailmsg.Message{
		From:      "alice@example.com",
		To:        []string{"bob@example.com"},
		Subject:   "hi",
		Body:      "secret",
		Mode:      "plain",
		Autocrypt: "addr=alice@example.com; keydata=" + keydataB64,
	}.Build()

	encrypted, err := pgpmail.EncryptMIME(plaintext, []string{alice.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	assertNoOverlongLines(t, encrypted)

	msg, err := mail.ReadMessage(bytes.NewReader(encrypted))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	_, gotKeydata, err := pgpautocrypt.ParseAutocryptHeader(msg.Header.Get("Autocrypt"))
	if err != nil {
		t.Fatalf("ParseAutocryptHeader: %v", err)
	}
	if !bytes.Equal(gotKeydata, keyBytes) {
		t.Fatalf("keydata round-trip mismatch on encrypted envelope")
	}
}

func TestEncryptStoredMIMEPreservesOriginalAndReadableAttachments(t *testing.T) {
	identity, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	original := mailmsg.Message{From: "bob@example.com", To: []string{"alice@example.com"}, Subject: "secret subject", Body: "secret body", Attachments: []mailmsg.Attachment{{Name: "report.txt", MimeType: "text/plain", Content: []byte("attachment contents")}}, MessageID: "<original@example.com>", Date: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}.Build()
	original = append([]byte("DKIM-Signature: preserved-evidence\r\nReceived: original trace\r\n"), original...)
	encrypted, err := pgpmail.EncryptStoredMIME(original, identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("secret subject")) || bytes.Contains(encrypted, []byte("secret body")) || bytes.Contains(encrypted, []byte("attachment contents")) {
		t.Fatal("plaintext escaped encryption")
	}
	if !bytes.Contains(encrypted, []byte("Message-ID: <original@example.com>")) || !bytes.Contains(encrypted, []byte("Date: Fri, 02 Oct 2026")) {
		t.Fatal("threading/date headers missing")
	}
	payload, ok := extractOctetStreamPart(t, encrypted)
	if !ok {
		t.Fatal("no OpenPGP payload")
	}
	result, err := pgpmailtest.DecryptMIME(payload, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.Content, original) {
		t.Fatal("complete original RFC 5322 bytes were not preserved")
	}
	subject, ok := pgpmailtest.ExtractProtectedSubject(result.Content)
	if !ok || subject != "secret subject" {
		t.Fatalf("subject=%q present=%v", subject, ok)
	}
	body, _, attachments, err := pgpmailtest.ParseContent(result.Content)
	if err != nil || body != "secret body" || len(attachments) != 1 || string(attachments[0].Content) != "attachment contents" {
		t.Fatalf("body=%q attachments=%v err=%v", body, attachments, err)
	}
}
