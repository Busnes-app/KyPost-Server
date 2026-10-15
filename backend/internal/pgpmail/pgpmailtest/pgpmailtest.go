// Package pgpmailtest holds the OpenPGP operations only tests perform.
// Production never generates keys, signs a whole message, decrypts or parses a
// decrypted entity: the browser does all of that. Tests use these to build
// fixtures and to read back what production code encrypted.
package pgpmailtest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

// GenerateIdentity creates a keypair whose primary User ID is email and which
// carries each distinct non-blank additionalEmails entry as a further User ID.
func GenerateIdentity(name, email string, additionalEmails ...string) (*pgpmail.Identity, error) {
	gen := crypto.PGP().KeyGeneration().AddUserId(name, email)
	seen := map[string]bool{normalize(email): true}
	for _, extra := range additionalEmails {
		addr := normalize(extra)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		gen = gen.AddUserId(name, addr)
	}
	key, err := gen.New().GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("pgpmailtest: generate key: %w", err)
	}
	armored, err := key.Armor()
	if err != nil {
		return nil, err
	}
	return pgpmail.ParsePrivateKey(armored)
}

func normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func privateKey(id *pgpmail.Identity) (*crypto.Key, error) {
	if id == nil {
		return nil, errors.New("pgpmailtest: identity required")
	}
	armored, err := id.ExportArmoredPrivateKey()
	if err != nil {
		return nil, err
	}
	return crypto.NewKeyFromArmored(armored)
}

func keyRing(armoredKeys []string) (*crypto.KeyRing, error) {
	ring, err := crypto.NewKeyRing(nil)
	if err != nil {
		return nil, err
	}
	for _, armored := range armoredKeys {
		key, err := crypto.NewKeyFromArmored(armored)
		if err != nil {
			return nil, err
		}
		if err := ring.AddKey(key); err != nil {
			return nil, err
		}
	}
	return ring, nil
}

// envelopeHeaders are the outer headers SignMIME keeps, in pgpmail's order.
var envelopeHeaders = []string{"From", "To", "Cc", "Bcc", "Subject", "Autocrypt", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References"}

// SignMIME wraps a message built by mailmsg.Message.Build in an RFC 3156
// multipart/signed envelope: its Content-Type and body, byte-identical, beside
// a detached signature over those exact bytes.
func SignMIME(plaintext []byte, signer *pgpmail.Identity) ([]byte, error) {
	key, err := privateKey(signer)
	if err != nil {
		return nil, err
	}
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(plaintext)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && header == nil {
		return nil, err
	}
	body, err := io.ReadAll(reader.R)
	if err != nil {
		return nil, err
	}
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain; charset=UTF-8"
	}
	var content bytes.Buffer
	content.WriteString("Content-Type: " + contentType + "\r\n")
	if cte := header.Get("Content-Transfer-Encoding"); cte != "" {
		content.WriteString("Content-Transfer-Encoding: " + cte + "\r\n")
	}
	content.WriteString("\r\n")
	content.Write(body)

	handle, err := crypto.PGP().Sign().SigningKey(key).Detached().New()
	if err != nil {
		return nil, err
	}
	signature, err := handle.Sign(content.Bytes(), crypto.Armor)
	if err != nil {
		return nil, err
	}

	boundary := "pgpmailtest-" + rand.Text()
	var msg bytes.Buffer
	for _, name := range envelopeHeaders {
		v := header.Get(name)
		switch name {
		case "Autocrypt":
			v = mailmsg.FoldHeaderValue(v)
		case "Subject":
			v = mailmsg.FoldEncodedWords(v)
		}
		if v != "" {
			msg.WriteString(name + ": " + v + "\r\n")
		}
	}
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString(`Content-Type: multipart/signed; protocol="application/pgp-signature"; micalg="pgp-sha256"; boundary="` + boundary + "\"\r\n\r\n")
	// The CRLF before each delimiter belongs to the delimiter (RFC 2046), so it
	// follows the signed content even when the content ends in its own CRLF.
	msg.WriteString("--" + boundary + "\r\n")
	msg.Write(content.Bytes())
	msg.WriteString("\r\n--" + boundary + "\r\n")
	msg.WriteString("Content-Type: application/pgp-signature; name=\"signature.asc\"\r\n")
	msg.WriteString("Content-Disposition: attachment; filename=\"signature.asc\"\r\n\r\n")
	msg.Write(signature)
	msg.WriteString("\r\n--" + boundary + "--\r\n")
	return msg.Bytes(), nil
}

// DecryptResult is a decrypted payload and, when signer keys were given, the
// embedded signature's verdict.
type DecryptResult struct {
	Content           []byte
	Signed            bool
	Verified          bool
	SignerFingerprint string
}

// DecryptMIME decrypts an armored OpenPGP message with recipient's key,
// verifying any embedded signature against signerArmoredPubKeys.
func DecryptMIME(armoredPGPMessage string, recipient *pgpmail.Identity, signerArmoredPubKeys []string) (*DecryptResult, error) {
	key, err := privateKey(recipient)
	if err != nil {
		return nil, err
	}
	keys, err := crypto.NewKeyRing(key)
	if err != nil {
		return nil, err
	}
	builder := crypto.PGP().Decryption().DecryptionKeys(keys)
	if len(signerArmoredPubKeys) > 0 {
		verify, err := keyRing(signerArmoredPubKeys)
		if err != nil {
			return nil, err
		}
		builder = builder.VerificationKeys(verify)
	}
	handle, err := builder.New()
	if err != nil {
		return nil, err
	}
	result, err := handle.Decrypt([]byte(armoredPGPMessage), crypto.Auto)
	if err != nil {
		return nil, fmt.Errorf("pgpmailtest: decrypt: %w", err)
	}
	out := &DecryptResult{Content: result.Bytes()}
	if len(signerArmoredPubKeys) > 0 {
		out.Signed = len(result.Signatures) > 0
		out.Verified = out.Signed && result.SignatureError() == nil
		if signer := result.SignedByKey(); signer != nil {
			out.SignerFingerprint = signer.GetFingerprint()
		}
	}
	return out, nil
}

// ExtractProtectedSubject reads the protected Subject from a decrypted entity,
// RFC 2047-decoded. ok is false when there is none.
func ExtractProtectedSubject(content []byte) (subject string, ok bool) {
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(content))).ReadMIMEHeader()
	if err != nil && header == nil {
		return "", false
	}
	raw := header.Get("Subject")
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(raw)
	if err != nil {
		decoded = raw
	}
	decoded = mailmsg.SanitizeHeaderValue(decoded)
	return decoded, decoded != ""
}

// Body render modes, matching the inbox wire values.
const (
	BodyModeHTML  = "html"
	BodyModePlain = "plain"
)

// maxContentDepth bounds recursion into nested multiparts.
const maxContentDepth = 8

// ParseContent decodes a decrypted PGP/MIME entity into its display body, the
// body's render mode and its attachments: the first unnamed text/plain,
// text/html or untyped part in document order is the body, text/rfc822-headers
// parts are skipped, anything else is an attachment.
//
// It is the reference frontend/src/lib/mimeContent.ts is held to by
// testdata/mime-corpus.json (see mime_corpus_test.go): a disagreement between
// two readers of one ciphertext lets one signature authenticate two messages.
// Change one, change both.
func ParseContent(content []byte) (body, mode string, attachments []mailmsg.Attachment, err error) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(content)))
	header, err := reader.ReadMIMEHeader()
	if err != nil && header == nil {
		return "", "", nil, fmt.Errorf("pgpmailtest: read content headers: %w", err)
	}
	rest, err := io.ReadAll(reader.R)
	if err != nil {
		return "", "", nil, err
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		rest = decodeBody(rest, header.Get("Content-Transfer-Encoding"))
		return string(rest), bodyMode(header.Get("Content-Type")), nil, nil
	}
	mode = BodyModePlain
	err = parseMultipart(bytes.NewReader(rest), params["boundary"], 0, &body, &mode, &attachments)
	return body, mode, attachments, err
}

func bodyMode(contentType string) string {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && strings.EqualFold(mediaType, "text/html") {
		return BodyModeHTML
	}
	return BodyModePlain
}

// partFileName honours Content-Type name= as well as Content-Disposition, as
// the browser does; part.FileName alone reads only the latter.
func partFileName(part *multipart.Part) string {
	if name := part.FileName(); name != "" {
		return name
	}
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return params["name"]
}

func parseMultipart(r io.Reader, boundary string, depth int, body, mode *string, attachments *[]mailmsg.Attachment) error {
	if depth >= maxContentDepth {
		return nil
	}
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("pgpmailtest: read multipart part: %w", err)
		}
		partType := part.Header.Get("Content-Type")
		// Classify on the parsed, case-insensitive media type (RFC 2045 5.1).
		partMedia, params, mtErr := mime.ParseMediaType(partType)
		if mtErr == nil && strings.HasPrefix(partMedia, "multipart/") && params["boundary"] != "" {
			if err := parseMultipart(part, params["boundary"], depth+1, body, mode, attachments); err != nil {
				return err
			}
			continue
		}
		partBody, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("pgpmailtest: read part body: %w", err)
		}
		partBody = decodeBody(partBody, part.Header.Get("Content-Transfer-Encoding"))
		if strings.EqualFold(partMedia, "text/rfc822-headers") {
			continue
		}
		filename := partFileName(part)
		if filename == "" && (strings.EqualFold(partMedia, "text/plain") || strings.EqualFold(partMedia, "text/html") || partType == "") {
			if *body == "" {
				*body = string(partBody)
				*mode = bodyMode(partType)
			}
			continue
		}
		if filename == "" {
			filename = "attachment"
		}
		*attachments = append(*attachments, mailmsg.Attachment{Name: filename, MimeType: partType, Content: partBody})
	}
}

// decodeBody reverses a Content-Transfer-Encoding, keeping the raw bytes when
// they do not decode, as the browser does. mime/multipart already decodes
// quoted-printable parts, so only a single-part entity reaches that case.
func decodeBody(body []byte, encoding string) []byte {
	var r io.Reader
	switch {
	case strings.EqualFold(encoding, "base64"):
		r = base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body))
	case strings.EqualFold(encoding, "quoted-printable"):
		r = quotedprintable.NewReader(bytes.NewReader(body))
	default:
		return body
	}
	decoded, err := io.ReadAll(r)
	if err != nil {
		return body
	}
	return decoded
}
