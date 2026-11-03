package config

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Signature bounds. A signature is a few lines of text; the caps keep one
// account's settings file small and the PUT body bounded.
const (
	MaxSignatures         = 10
	MaxSignatureNameRunes = 60
	MaxSignatureTextRunes = 2000
	MaxSignatureAddresses = 100
)

// Signature is one saved email signature. Text is PLAIN TEXT, stored and
// served verbatim: every client renders it as text (the web composer escapes
// it into paragraphs), so markup typed into it is shown, never interpreted.
// There is deliberately no HTML field — nothing here can carry script into a
// composer or a recipient's mail client.
type Signature struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	Text string `yaml:"text" json:"text"`
}

// UserSignatureSettings is the account's signature set, shared by every client
// (GET /api/mail/signatures). Which signature compose inserts:
//
//   - ByAddress[from] when the From address has an entry; "" there means "no
//     signature for this address", overriding the default;
//   - otherwise DefaultID ("" means none).
//
// OnReply and OnForward say whether that signature is also inserted when
// replying or forwarding; a brand-new message always gets it.
type UserSignatureSettings struct {
	Signatures []Signature       `yaml:"signatures" json:"signatures"`
	DefaultID  string            `yaml:"defaultId" json:"defaultId"`
	ByAddress  map[string]string `yaml:"byAddress" json:"byAddress"`
	OnReply    bool              `yaml:"onReply" json:"onReply"`
	OnForward  bool              `yaml:"onForward" json:"onForward"`
}

// DefaultUserSignatureSettings has no signatures. Replies and forwards are on
// so that a signature, once the user writes one, behaves as they expect from
// other mail clients; with no signature they change nothing.
func DefaultUserSignatureSettings() UserSignatureSettings {
	return UserSignatureSettings{Signatures: []Signature{}, ByAddress: map[string]string{}, OnReply: true, OnForward: true}
}

var signatureIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// isBidiControl reports the explicit bidirectional embedding, override and
// isolate controls. They are invisible and reorder the text around them, so a
// signature carrying one can make a recipient read something other than what
// the sender's settings page shows.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// NormalizeSignatureSettings validates s and returns its canonical form:
// names trimmed, line endings folded to \n, address keys lowercased. Anything
// it cannot make canonical is refused with an error naming the problem, which
// the API returns to the caller as a 400.
func NormalizeSignatureSettings(s UserSignatureSettings) (UserSignatureSettings, error) {
	out := UserSignatureSettings{Signatures: []Signature{}, ByAddress: map[string]string{}, OnReply: s.OnReply, OnForward: s.OnForward}
	if len(s.Signatures) > MaxSignatures {
		return out, fmt.Errorf("at most %d signatures", MaxSignatures)
	}
	ids := map[string]bool{}
	for _, sig := range s.Signatures {
		if !signatureIDPattern.MatchString(sig.ID) {
			return out, errors.New("signature id must be 1-64 letters, digits, - or _")
		}
		if ids[sig.ID] {
			return out, errors.New("duplicate signature id")
		}
		ids[sig.ID] = true
		name := strings.TrimSpace(sig.Name)
		switch {
		case name == "":
			return out, errors.New("every signature needs a name")
		case !utf8.ValidString(name):
			return out, errors.New("signature name is not valid UTF-8")
		case utf8.RuneCountInString(name) > MaxSignatureNameRunes:
			return out, fmt.Errorf("signature name longer than %d characters", MaxSignatureNameRunes)
		case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || isBidiControl(r) }):
			return out, errors.New("signature name contains control characters")
		}
		text := strings.ReplaceAll(sig.Text, "\r\n", "\n")
		switch {
		case !utf8.ValidString(text):
			return out, errors.New("signature text is not valid UTF-8")
		case utf8.RuneCountInString(text) > MaxSignatureTextRunes:
			return out, fmt.Errorf("signature longer than %d characters", MaxSignatureTextRunes)
		case strings.ContainsFunc(text, func(r rune) bool {
			return (unicode.IsControl(r) && r != '\n' && r != '\t') || isBidiControl(r)
		}):
			return out, errors.New("signature text contains control characters")
		}
		out.Signatures = append(out.Signatures, Signature{ID: sig.ID, Name: name, Text: text})
	}
	if s.DefaultID != "" && !ids[s.DefaultID] {
		return out, errors.New("default signature does not exist")
	}
	out.DefaultID = s.DefaultID
	if len(s.ByAddress) > MaxSignatureAddresses {
		return out, fmt.Errorf("at most %d per-address signatures", MaxSignatureAddresses)
	}
	for addr, id := range s.ByAddress {
		key := strings.ToLower(strings.TrimSpace(addr))
		parsed, err := mail.ParseAddress(key)
		if err != nil || parsed.Address != key || len(key) > 254 {
			return out, fmt.Errorf("not an email address: %q", addr)
		}
		if _, dup := out.ByAddress[key]; dup {
			return out, fmt.Errorf("address listed twice: %q", key)
		}
		if id != "" && !ids[id] {
			return out, fmt.Errorf("signature for %s does not exist", key)
		}
		out.ByAddress[key] = id
	}
	return out, nil
}
