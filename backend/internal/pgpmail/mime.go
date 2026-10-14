package pgpmail

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// envelopeHeaderOrder lists the outer RFC 5322 headers preserved verbatim on a
// PGP/MIME-wrapped message, in the order mailmsg.Message.Build() writes them.
// PGP/MIME wraps only the content (Content-Type + body); OpenPGP does not
// protect routing metadata, so routing and display headers stay outside the
// encrypted/signed part. Autocrypt is here rather than folded into the encrypted
// content because it exists to let a receiving client opportunistically pick up
// the sender's public key without decrypting anything.
var envelopeHeaderOrder = []string{"From", "To", "Cc", "Bcc", "Subject", "Autocrypt", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References"}

// splitMessage separates a raw RFC 5322 message (as produced by
// mailmsg.Message.Build()) into its preserved envelope headers and its inner
// content part (the original Content-Type header plus body, byte-identical
// to the input).
func splitMessage(raw []byte) (envelope textproto.MIMEHeader, content []byte, err error) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && header == nil {
		return nil, nil, fmt.Errorf("pgpmail: read message headers: %w", err)
	}

	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain; charset=UTF-8"
	}

	var body bytes.Buffer
	if _, err := body.ReadFrom(reader.R); err != nil {
		return nil, nil, fmt.Errorf("pgpmail: read message body: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString("Content-Type: " + contentType + "\r\n")
	if transferEncoding := header.Get("Content-Transfer-Encoding"); transferEncoding != "" {
		buf.WriteString("Content-Transfer-Encoding: " + transferEncoding + "\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body.Bytes())

	return header, buf.Bytes(), nil
}

// writeEnvelopeHeaders refolds encoded Subjects and Autocrypt keydata after
// splitMessage unfolds them, preserving SMTP physical-line limits.
func writeEnvelopeHeaders(w io.Writer, envelope textproto.MIMEHeader) {
	for _, name := range envelopeHeaderOrder {
		v := envelope.Get(name)
		if v == "" {
			continue
		}
		switch name {
		case "Autocrypt":
			v = mailmsg.FoldHeaderValue(v)
		case "Subject":
			v = mailmsg.FoldEncodedWords(v)
		}
		_, _ = io.WriteString(w, name+": "+v+"\r\n")
	}
}

// OuterPlaceholderSubject replaces the real Subject on the unencrypted outer
// envelope of a PGP/MIME-encrypted message. The real subject is instead
// carried inside the encrypted payload via protected headers (protectContent)
// and restored by the reading client. Also reused
// for the outer subject of pickup notifications so the real subject never
// travels in cleartext.
const OuterPlaceholderSubject = "[Encrypted] Email Sent by KyPost"

// protectContent wraps an inner MIME content part (a Content-Type header line
// plus body, as produced by splitMessage) in a Protected Headers v1 structure —
// the "memoryhole" convention of draft-ietf-lamps-header-protection emitted and
// consumed by Thunderbird, Mutt and K-9. The real subject is copied onto the
// wrapper's own headers (protected-headers="v1") so an aware client shows it,
// and, when non-empty, into a leading text/rfc822-headers "legacy display" part
// so any other client still renders it readably. The original content is nested
// byte-verbatim as the final part.
//
// Scope is deliberately Subject-only (LAMPS baseline). Hand-assembled rather
// than via a mime/multipart.Writer: CreatePart injects its own header
// separator and would corrupt the byte-verbatim nested content.
func protectContent(content []byte, subject string) []byte {
	subject = mailmsg.FoldEncodedWords(mailmsg.SanitizeHeaderValue(subject))
	boundary := randomBoundary()

	var msg bytes.Buffer
	if subject != "" {
		msg.WriteString("Subject: " + subject + "\r\n")
	}
	msg.WriteString(`Content-Type: multipart/mixed; boundary="` + boundary + `"; protected-headers="v1"` + "\r\n")
	msg.WriteString("\r\n")

	if subject != "" {
		msg.WriteString("--" + boundary + "\r\n")
		msg.WriteString("Content-Type: text/rfc822-headers; protected-headers=\"v1\"\r\n")
		msg.WriteString("Content-Disposition: inline\r\n")
		msg.WriteString("\r\n")
		msg.WriteString("Subject: " + subject + "\r\n")
		msg.WriteString("\r\n")
	}

	// Per RFC 2046 the CRLF before a boundary delimiter belongs to the
	// delimiter; write it unconditionally after the (possibly multipart)
	// content.
	msg.WriteString("--" + boundary + "\r\n")
	msg.Write(content)
	msg.WriteString("\r\n")
	msg.WriteString("--" + boundary + "--\r\n")
	return msg.Bytes()
}

// EncryptMIME wraps a plaintext RFC 5322 message (as produced by
// mailmsg.Message.Build()) in an RFC 3156 multipart/encrypted envelope. The
// message's Content-Type and body become the encrypted payload; From/To/Cc/Bcc
// stay on the outer, unencrypted envelope headers. A Subject is moved into the
// encrypted payload via protected headers and the outer Subject replaced with
// OuterPlaceholderSubject, so the real subject never travels in cleartext. A
// non-nil signer signs the content before encryption, so the recipient
// verifies it while decrypting.
func EncryptMIME(plaintext []byte, recipientArmoredPubKeys []string, signer *Identity) ([]byte, error) {
	return encryptMIME(plaintext, recipientArmoredPubKeys, signer, false)
}

// EncryptStoredMIME preserves the complete original RFC 5322 bytes inside the
// encrypted MIME entity, including trace headers and sender signatures. It
// does not sign on behalf of the original sender.
func EncryptStoredMIME(plaintext []byte, publicKey string) ([]byte, error) {
	return encryptMIME(plaintext, []string{publicKey}, nil, true)
}

func encryptMIME(plaintext []byte, recipientArmoredPubKeys []string, signer *Identity, preserveOriginal bool) ([]byte, error) {
	if len(recipientArmoredPubKeys) == 0 {
		return nil, errors.New("pgpmail: at least one recipient key required")
	}
	envelope, content, err := splitMessage(plaintext)
	if err != nil {
		return nil, err
	}
	if preserveOriginal {
		content = plaintext
	}
	if realSubject := envelope.Get("Subject"); realSubject != "" {
		envelope.Set("Subject", OuterPlaceholderSubject)
		content = protectContent(content, realSubject)
	}

	recipients, err := crypto.NewKeyRing(nil)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: new recipient keyring: %w", err)
	}
	for _, armored := range recipientArmoredPubKeys {
		key, err := crypto.NewKeyFromArmored(armored)
		if err != nil {
			return nil, fmt.Errorf("pgpmail: parse recipient key: %w", err)
		}
		if err := recipients.AddKey(key); err != nil {
			return nil, fmt.Errorf("pgpmail: add recipient key: %w", err)
		}
	}

	builder := crypto.PGP().Encryption().Recipients(recipients)
	if signer != nil {
		builder = builder.SigningKey(signer.key)
	}
	encHandle, err := builder.New()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: build encryption handle: %w", err)
	}
	pgpMessage, err := encHandle.Encrypt(content)
	if err != nil {
		return nil, fmt.Errorf("pgpmail: encrypt: %w", err)
	}
	armored, err := pgpMessage.Armor()
	if err != nil {
		return nil, fmt.Errorf("pgpmail: armor encrypted message: %w", err)
	}

	return buildEncryptedEnvelope(envelope, armored), nil
}

func buildEncryptedEnvelope(envelope textproto.MIMEHeader, armoredEncrypted string) []byte {
	var msg bytes.Buffer
	writeEnvelopeHeaders(&msg, envelope)
	msg.WriteString("MIME-Version: 1.0\r\n")

	w := multipart.NewWriter(&msg)
	msg.WriteString(`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary=` + w.Boundary() + "\r\n")
	msg.WriteString("\r\n")

	control, _ := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/pgp-encrypted"}})
	_, _ = io.WriteString(control, "Version: 1\r\n")

	data, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {`application/octet-stream; name="encrypted.asc"`},
		"Content-Disposition": {`inline; filename="encrypted.asc"`},
	})
	_, _ = io.WriteString(data, armoredEncrypted)

	_ = w.Close()
	return msg.Bytes()
}

// randomBoundary returns a MIME boundary with 128+ bits of entropy.
//
// crypto/rand.Text is used rather than rand.Read so there is no error to
// discard: it panics if the system CSPRNG fails, which is the contract this
// code wants — a boundary built from a failed read would be a constant, and a
// predictable boundary in a signed message is an attacker-chosen delimiter.
// Its alphabet (base32, A-Z2-7) is valid in a boundary unquoted.
func randomBoundary() string {
	return "pgpmail-" + rand.Text()
}

// Detached-signature verification lives in the BROWSER, not here — see
// api.markSignedOnlyMessageContent. An exported VerifyDetached used to sit at
// this spot and its only caller fed it go-imap's decoded body, which is not
// what a detached signature covers, so it reported every signed message as
// unverified. Recovering the bytes it would have needed is
// ExtractSignedParts's job (signed_parts.go), and by the time you have them
// the verification belongs on the client that fetched them.
