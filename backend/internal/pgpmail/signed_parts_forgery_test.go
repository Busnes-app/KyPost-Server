package pgpmail_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// buildThreePartSigned wraps a genuine two-part signed message in a third part
// the signature does not cover, in the given order.
//
// This is the shape behind CVE-2021-4126: an extra MIME part alongside a valid
// signed pair. Go's enmime picks the extra part as the display body while a
// signature-scoped extractor returns part 1, so a reader that trusts the
// extractor's verdict and enmime's body shows attacker content under a
// "signature verified" badge.
func buildThreePartSigned(t *testing.T, order string) []byte {
	t.Helper()
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	plaintext := mailmsg.Message{
		From:    "alice@example.com",
		To:      []string{"bob@example.com"},
		Subject: "Signed",
		Body:    "the real signed text",
		Mode:    "plain",
	}.Build()
	signed, err := pgpmailtest.SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}

	// The genuine two-part message passes the extractor; the forgery is built
	// from its pieces.
	part, sig, err := pgpmail.ExtractSignedParts(signed)
	if err != nil {
		t.Fatalf("could not split the genuine message: %v", err)
	}

	const b = "OUTER"
	attacker := "Content-Type: text/html; charset=utf-8\r\n\r\n<p>Pay the attacker</p>"
	sigPart := "Content-Type: application/pgp-signature; name=\"signature.asc\"\r\n" +
		"Content-Disposition: attachment; filename=\"signature.asc\"\r\n\r\n" + sig

	var body strings.Builder
	body.WriteString("Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; boundary=\"" + b + "\"\r\n")
	body.WriteString("From: alice@example.com\r\n\r\n")

	write := func(s string) {
		body.WriteString("--" + b + "\r\n")
		body.WriteString(s)
		body.WriteString("\r\n")
	}
	switch order {
	case "attacker-last":
		write(string(part))
		write(sigPart)
		write(attacker)
	case "attacker-middle":
		// Defeats a fix that only looks for content AFTER the signature part.
		write(string(part))
		write(attacker)
		write(sigPart)
	default:
		t.Fatalf("unknown order %q", order)
	}
	body.WriteString("--" + b + "--\r\n")
	return []byte(body.String())
}

// A multipart/signed message carries exactly two parts (RFC 1847 2.1). A third
// part is not covered by the signature, and accepting one is what lets a green
// badge appear over content nobody signed.
func TestExtractSignedPartsRejectsAThirdPart(t *testing.T) {
	for _, order := range []string{"attacker-last", "attacker-middle"} {
		t.Run(order, func(t *testing.T) {
			raw := buildThreePartSigned(t, order)
			_, _, err := pgpmail.ExtractSignedParts(raw)
			if !errors.Is(err, pgpmail.ErrNotSignedMessage) {
				t.Fatalf("a three-part multipart/signed must be refused, got err=%v", err)
			}
		})
	}
}

// The delimiter line may carry transport padding and nothing else (RFC 2046
// 5.1.1). Accepting arbitrary trailing junk makes this scanner disagree with
// every conforming parser about where the signed part begins.
func TestExtractSignedPartsRejectsJunkAfterTheDelimiter(t *testing.T) {
	alice, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	plaintext := mailmsg.Message{
		From: "alice@example.com", To: []string{"bob@example.com"},
		Subject: "Signed", Body: "trust me", Mode: "plain",
	}.Build()
	signed, err := pgpmailtest.SignMIME(plaintext, alice)
	if err != nil {
		t.Fatalf("SignMIME: %v", err)
	}

	var boundary string
	if i := bytes.Index(signed, []byte("boundary=\"")); i >= 0 {
		rest := signed[i+len("boundary=\""):]
		boundary = string(rest[:bytes.IndexByte(rest, '"')])
	}
	if boundary == "" {
		t.Fatal("could not read the boundary")
	}

	t.Run("junk on the opening delimiter", func(t *testing.T) {
		forged := bytes.Replace(signed,
			[]byte("\r\n--"+boundary+"\r\n"),
			[]byte("\r\n--"+boundary+"JUNK\r\n"), 1)
		if _, _, err := pgpmail.ExtractSignedParts(forged); !errors.Is(err, pgpmail.ErrNotSignedMessage) {
			t.Fatalf("junk after the boundary must be refused, got err=%v", err)
		}
	})

	t.Run("a boundary that is a prefix of the real one", func(t *testing.T) {
		short := boundary[:len(boundary)-2]
		forged := bytes.Replace(signed,
			[]byte("boundary=\""+boundary+"\""),
			[]byte("boundary=\""+short+"\""), 1)
		if _, _, err := pgpmail.ExtractSignedParts(forged); !errors.Is(err, pgpmail.ErrNotSignedMessage) {
			t.Fatalf("a prefix boundary must be refused, got err=%v", err)
		}
	})

	t.Run("transport padding is still accepted", func(t *testing.T) {
		padded := bytes.Replace(signed,
			[]byte("\r\n--"+boundary+"\r\n"),
			[]byte("\r\n--"+boundary+" \t\r\n"), 1)
		part, _, err := pgpmail.ExtractSignedParts(padded)
		if err != nil {
			t.Fatalf("RFC 2046 permits LWSP after the boundary: %v", err)
		}
		if len(part) == 0 {
			t.Fatal("expected the signed part")
		}
	})
}
