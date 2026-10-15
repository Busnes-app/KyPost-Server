package pgpmail_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// verifyDetached fails the test unless armoredSignature verifies over exactly
// data, which is how a reader checks a multipart/signed message.
func verifyDetached(t *testing.T, data []byte, armoredSignature string, signer *pgpmail.Identity) {
	t.Helper()
	key, err := crypto.NewKeyFromArmored(signer.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewKeyRing(key)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := crypto.PGP().Verify().VerificationKeys(keys).New()
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.VerifyDetached(data, []byte(armoredSignature), crypto.Auto)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.SignatureError(); err != nil {
		t.Fatalf("signature does not verify over the extracted part: %v", err)
	}
}

// The signed part ExtractSignedParts returns must be byte-identical to the
// signed content. Anything less and verification cannot succeed — which is
// exactly the production bug this function exists to fix, where the caller
// passed go-imap's DECODED body instead.
func TestExtractSignedPartsRoundTripsSignMIME(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: strings.TrimSpace(strings.Repeat("日本語 ✉ ", 40)),
		Body:    "trust me",
		Mode:    "plain",
	}.Build()

	signed, err := pgpmailtest.SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}

	gotPart, gotSig, err := pgpmail.ExtractSignedParts(signed)
	if err != nil {
		t.Fatalf("ExtractSignedParts: %v", err)
	}
	// The body travels base64: "dHJ1c3QgbWU=" is "trust me".
	if !bytes.HasPrefix(gotPart, []byte("Content-Type: text/plain")) || !bytes.Contains(gotPart, []byte("dHJ1c3QgbWU=")) {
		t.Fatalf("signed part is not the message content: %q", gotPart)
	}
	if !strings.HasPrefix(gotSig, "-----BEGIN PGP SIGNATURE-----") ||
		!strings.HasSuffix(gotSig, "-----END PGP SIGNATURE-----") {
		t.Fatalf("expected a complete armored signature block, got %q", gotSig)
	}
	verifyDetached(t, gotPart, gotSig, alice)
}

// A multipart/mixed body (message with an attachment) ends in its own CRLF.
// The extractor must cut at the CRLF that belongs to the boundary delimiter
// and not one byte further.
func TestExtractSignedPartsPreservesTrailingCRLF(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Signed with attachment",
		Body:    "see attached",
		Mode:    "plain",
		Attachments: []mailmsg.Attachment{
			{Name: "note.txt", MimeType: "text/plain", Content: []byte("hello file")},
		},
	}.Build()

	signed, err := pgpmailtest.SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}

	gotPart, gotSig, err := pgpmail.ExtractSignedParts(signed)
	if err != nil {
		t.Fatalf("ExtractSignedParts: %v", err)
	}
	// The inner multipart's close delimiter and its own CRLF are signed data.
	if !bytes.HasSuffix(gotPart, []byte("--\r\n")) {
		t.Fatalf("trailing CRLF handling is wrong: part ends %q", gotPart[max(0, len(gotPart)-20):])
	}
	verifyDetached(t, gotPart, gotSig, alice)
}

func TestExtractSignedPartsRejectsNonSignedMessages(t *testing.T) {
	cases := map[string][]byte{
		"plain text message": []byte("From: a@example.com\r\n" +
			"Content-Type: text/plain\r\n\r\nhello\r\n"),
		"multipart/mixed, not signed": []byte("From: a@example.com\r\n" +
			"Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n"),
		"multipart/signed with a non-PGP protocol": []byte("From: a@example.com\r\n" +
			"Content-Type: multipart/signed; protocol=\"application/pkcs7-signature\"; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n"),
		"multipart/signed with no signature armor": []byte("From: a@example.com\r\n" +
			"Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
			"--b\r\nContent-Type: application/pgp-signature\r\n\r\nnot armor\r\n--b--\r\n"),
		"multipart/signed with only one part": []byte("From: a@example.com\r\n" +
			"Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b--\r\n"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := pgpmail.ExtractSignedParts(raw); !errors.Is(err, pgpmail.ErrNotSignedMessage) {
				t.Fatalf("expected ErrNotSignedMessage, got %v", err)
			}
		})
	}
}
