package imap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// run-4 findings H3 and M10: a DKIM pass proves only that the headers the
// signature actually covered are intact. RFC 6376 hashes just the headers
// named in h=, takes the LAST occurrence of each, and tolerates extra fields —
// so an attacker can staple an unsigned duplicate above a signed one, or
// replace a header the signer never covered, and the signature still verifies.
//
// Send-as verification trusted a Subject located by IMAP SEARCH, and Autocrypt
// harvest trusted an Autocrypt header, on the strength of a d= match alone.
// Neither header need have been signed.
func TestVerifyDKIMCoversHeader(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	lookup := func(string) ([]string, error) {
		return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}

	const body = "From: newsletter@bank.com\r\n" +
		"To: mallory@evil.test\r\n" +
		"Subject: Your monthly statement\r\n" +
		"Date: Sat, 26 Jul 2026 10:00:00 +0000\r\n" +
		"\r\nHello.\r\n"

	sign := func(headers []string) string {
		var out strings.Builder
		if err := dkim.Sign(&out, strings.NewReader(body), &dkim.SignOptions{
			Domain: "bank.com", Selector: "sel", Signer: priv, HeaderKeys: headers,
		}); err != nil {
			t.Fatalf("Sign: %v", err)
		}
		return out.String()
	}

	t.Run("unsigned header replaced", func(t *testing.T) {
		// Subject absent from h=, so the attacker simply rewrites it.
		signed := sign([]string{"from", "to", "date"})
		tampered := strings.Replace(signed, "Subject: Your monthly statement", "Subject: KYPOST-VERIFY-9f3a21", 1)
		if verifyDKIMCoversHeaderWithLookup([]byte(tampered), "bank.com", "Subject", lookup) {
			t.Error("accepted a Subject the signature never covered")
		}
	})

	t.Run("duplicate header prepended above the signed one", func(t *testing.T) {
		// Subject signed once (go-msgauth's default, and what most senders do).
		// The verifier hashes the last occurrence; IMAP SEARCH matches the
		// attacker's prepended one.
		signed := sign([]string{"from", "to", "subject", "date"})
		tampered := "Subject: KYPOST-VERIFY-9f3a21\r\n" + signed
		if verifyDKIMCoversHeaderWithLookup([]byte(tampered), "bank.com", "Subject", lookup) {
			t.Error("accepted a message carrying a second, unsigned Subject above the signed one")
		}
	})

	t.Run("genuinely signed single header", func(t *testing.T) {
		signed := sign([]string{"from", "to", "subject", "date"})
		if !verifyDKIMCoversHeaderWithLookup([]byte(signed), "bank.com", "Subject", lookup) {
			t.Error("rejected a Subject that the signature does cover")
		}
	})

	t.Run("wrong domain", func(t *testing.T) {
		signed := sign([]string{"from", "to", "subject", "date"})
		if verifyDKIMCoversHeaderWithLookup([]byte(signed), "other.example", "Subject", lookup) {
			t.Error("accepted a signature from a domain that is not the one asked about")
		}
	})
}

func signForTest(t *testing.T, f *dkimTestFixture, mail string, canon dkim.Canonicalization, headers ...string) string {
	t.Helper()
	var b strings.Builder
	if err := dkim.Sign(&b, strings.NewReader(mail), &dkim.SignOptions{
		Domain: f.domain, Selector: f.selector, Signer: f.key,
		HeaderCanonicalization: canon, HeaderKeys: headers,
	}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return b.String()
}

// DKIMSignedHeaders returns the signed instance of each covered header, and
// nothing for a header the signature left out.
func TestDKIMSignedHeaders(t *testing.T) {
	f := newDKIMTestFixture(t, "example.com", "sel")
	mail := strings.Replace(testMail, "Subject: hello", "Subject: hello\r\n world", 1)
	signed := signForTest(t, f, mail, dkim.CanonicalizationRelaxed, "from", "subject", "message-id")

	got := DKIMSignedHeadersWithLookup([]byte(signed), "example.com", f.lookupTXT)
	if got["from"] != "alice@example.com" || got["subject"] != "hello world" || got["message-id"] != "<test@example.com>" {
		t.Fatalf("signed values = %q", got)
	}
	if _, ok := got["date"]; ok {
		t.Fatalf("unsigned Date reported as signed: %q", got)
	}
	if len(DKIMSignedHeadersWithLookup([]byte(signed), "other.example", f.lookupTXT)) != 0 {
		t.Fatal("headers reported for a domain that did not sign")
	}
}

// The verifier matches field names after trimming whitespace, so "Subject :"
// is another Subject to it. Any second instance, however it is spelled and
// wherever it sits, must leave the header uncovered: otherwise a reader can
// take the unsigned copy's value for the signed one.
func TestDKIMSignedHeadersRefusesAnyDuplicate(t *testing.T) {
	f := newDKIMTestFixture(t, "example.com", "sel")
	for _, canon := range []dkim.Canonicalization{dkim.CanonicalizationRelaxed, dkim.CanonicalizationSimple} {
		signed := signForTest(t, f, testMail, canon, "from", "subject", "message-id", "date")
		for _, extra := range []string{
			"Subject: forged", "Subject : forged", "Subject\t: forged", "SUBJECT : forged",
			"Message-ID : <forged@example.com>", "From : mallory@example.com", "Date : Mon, 1 Jan 2035 00:00:00 +0000",
		} {
			name := strings.ToLower(strings.TrimSpace(strings.SplitN(extra, ":", 2)[0]))
			for where, raw := range map[string]string{
				"above": extra + "\r\n" + signed,
				"below": strings.Replace(signed, "\r\n\r\n", "\r\n"+extra+"\r\n\r\n", 1),
			} {
				if v, ok := DKIMSignedHeadersWithLookup([]byte(raw), "example.com", f.lookupTXT)[name]; ok {
					t.Errorf("%s %s %q: %s reported covered as %q", canon, where, extra, name, v)
				}
			}
		}
	}

	// The exploit shape: relaxed canonicalization ignores whitespace before the
	// colon, so the signed line can be respelled "Subject :" (net/mail then
	// drops it) and an unsigned plain Subject prepended (net/mail reads that).
	relaxed := signForTest(t, f, testMail, dkim.CanonicalizationRelaxed, "from", "subject", "message-id", "date")
	respelled := "Subject: forged\r\n" + strings.Replace(relaxed, "\r\nSubject: hello", "\r\nSubject : hello", 1)
	if v, ok := DKIMSignedHeadersWithLookup([]byte(respelled), "example.com", f.lookupTXT)["subject"]; ok {
		t.Errorf("respelled signed Subject plus unsigned plain one reported covered as %q", v)
	}

	// Both instances signed (h= names it twice) is still two values: refuse.
	twice := strings.Replace(testMail, "Subject: hello", "Subject: one\r\nSubject: two", 1)
	signed := signForTest(t, f, twice, dkim.CanonicalizationRelaxed, "from", "subject", "subject")
	if v, ok := DKIMSignedHeadersWithLookup([]byte(signed), "example.com", f.lookupTXT)["subject"]; ok {
		t.Errorf("two signed Subjects reported covered as %q", v)
	}
}
