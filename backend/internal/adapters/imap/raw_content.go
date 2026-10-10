package imap

import (
	"bytes"
	"errors"
	"net/mail"
	"regexp"
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

// maxCalendarParts bounds how many undisposed calendar parts are exposed.
const maxCalendarParts = 4

// calendarParts exposes the text/calendar parts enmime leaves in OtherParts —
// an iMIP invite sent as a multipart/alternative body part has no
// Content-Disposition — as attachments named invite.ics. They go after every
// real attachment, so existing attachment indexes do not move.
func calendarParts(env *enmime.Envelope) []goimap.Attachment {
	var out []goimap.Attachment
	for _, p := range env.OtherParts {
		if strings.EqualFold(p.ContentType, "text/calendar") && len(out) < maxCalendarParts {
			out = append(out, goimap.Attachment{Name: "invite.ics", MimeType: "text/calendar", Content: p.Content})
		}
	}
	return out
}

// calendarMethodPattern is an iTIP method name (RFC 5546 §1.4 plus x-names).
var calendarMethodPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)

// CalendarMethod returns the uppercase METHOD property of an iCalendar
// object, or "" when there is none or it is not a plain token. Only the head
// of the object is scanned: METHOD precedes the first component.
func CalendarMethod(ics []byte) string {
	if len(ics) > 64<<10 {
		ics = ics[:64<<10]
	}
	for _, line := range strings.Split(string(ics), "\n") {
		name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "METHOD":
			if value = strings.TrimSpace(value); calendarMethodPattern.MatchString(value) {
				return strings.ToUpper(value)
			}
			return ""
		case "BEGIN":
			if !strings.EqualFold(strings.TrimSpace(value), "VCALENDAR") {
				return ""
			}
		}
	}
	return ""
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
	e.Attachments = append(e.Attachments, calendarParts(env)...)
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
