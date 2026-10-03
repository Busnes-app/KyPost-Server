package imap

import (
	"bytes"
	"errors"
	"net/mail"
	"strings"

	goimap "github.com/BrianLeishman/go-imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/jhillyerd/enmime/v2"
)

var ErrMalformedMIME = errors.New("mail MIME cannot be parsed; original mail retained")

// ParsedContent reuses IMAP's rendering, signer binding and PGP candidate rules
// for permanent raw mail. It never decrypts or reconstructs signed MIME.
type ParsedContent struct {
	Content     MessageContent
	Text, HTML  string
	Attachments []mailmsg.Attachment
}

// OverviewFromHeaders decodes metadata without reading a stored body BLOB.
// Addresses use the same enmime parser as go-imap; only a single mailbox binds.
func OverviewFromHeaders(uid int, headers mail.Header) Overview {
	return overviewFromEmail(uid, emailFromHeaders(headers))
}
func emailFromHeaders(headers mail.Header) *goimap.Email {
	e := &goimap.Email{Subject: enmime.DecodeRFC2047(headers.Get("Subject"))}
	for _, field := range []struct {
		key    string
		target *goimap.EmailAddresses
	}{{"From", &e.From}, {"To", &e.To}, {"Cc", &e.CC}, {"Bcc", &e.BCC}} {
		addresses, _ := enmime.ParseAddressList(headers.Get(field.key))
		*field.target = make(goimap.EmailAddresses, len(addresses))
		for _, a := range addresses {
			(*field.target)[strings.ToLower(a.Address)] = a.Name
		}
	}
	e.Sent, _ = headers.Date()
	return e
}
func ParseRawContent(raw []byte) (ParsedContent, error) {
	if int64(len(raw)) > mailmsg.MaxInboundMessageBytes {
		return ParsedContent{}, mailmsg.ErrMessageTooLarge
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedContent{}, ErrMalformedMIME
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return ParsedContent{}, ErrMalformedMIME
	}
	e := emailFromHeaders(m.Header)
	e.Text, e.HTML = env.Text, env.HTML
	for _, parts := range [][]*enmime.Part{env.Attachments, env.Inlines} {
		for _, a := range parts {
			e.Attachments = append(e.Attachments, goimap.Attachment{Name: a.FileName, MimeType: a.ContentType, Content: a.Content})
		}
	}
	if emailContentSize(e) > mailmsg.MaxInboundMessageBytes {
		return ParsedContent{}, mailmsg.ErrMessageTooLarge
	}
	body, mode := clientBody(e)
	text, html := inboxBodies(e)
	payload := ""
	if body == "" && isPGPMIMERootContentType(m.Header.Get("Content-Type")) {
		payload = pgpEnvelopePayload(e.Attachments)
	}
	result := ParsedContent{Content: MessageContent{Body: body, BodyMode: mode, Sender: singleMailboxSender(e), HasAttachments: len(e.Attachments) > 0, PGPEncryptedPayload: payload, PGPEncrypted: payload != "", PGPSignaturePayload: ""}, Text: text, HTML: html, Attachments: []mailmsg.Attachment{}}
	if payload != "" {
		result.Content.HasAttachments = false
	} else if body != "" {
		result.Content.PGPSignaturePayload = pgpDetectSignature(e.Attachments)
	}
	for _, a := range e.Attachments {
		result.Attachments = append(result.Attachments, mailmsg.Attachment{Name: a.Name, MimeType: a.MimeType, Content: a.Content})
	}
	return result, nil
}
