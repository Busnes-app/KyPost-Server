package imap

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMAlignedFrom passes only a valid signature whose d= is the From
// address's own domain and whose h= includes From.
func TestDKIMAlignedFrom(t *testing.T) {
	example := newDKIMTestFixture(t, "example.com", "sel")
	other := newDKIMTestFixture(t, "other.net", "sel")
	sign := func(f *dkimTestFixture, mail string, headers []string) []byte {
		var b bytes.Buffer
		if err := dkim.Sign(&b, strings.NewReader(mail), &dkim.SignOptions{
			Domain: f.domain, Selector: f.selector, Signer: f.key, Hash: crypto.SHA256, HeaderKeys: headers,
		}); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	lookup := func(name string) ([]string, error) {
		if v, err := example.lookupTXT(name); err == nil {
			return v, nil
		}
		return other.lookupTXT(name)
	}
	// The hand-made signature is otherwise valid: it fails only for From.
	v, err := dkim.VerifyWithOptions(bytes.NewReader(signWithoutFrom(t, example, testMail)), &dkim.VerifyOptions{LookupTXT: lookup})
	if err != nil || len(v) != 1 || v[0].Err == nil || !strings.Contains(v[0].Err.Error(), "From field not signed") {
		t.Fatalf("unsigned-From fixture fails for the wrong reason: %v %+v", err, v)
	}
	named := strings.Replace(testMail, "From: alice@example.com", "From: Alice <Alice@Example.com>", 1)
	all := []string{"From", "To", "Subject", "Date", "Message-ID"}
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"aligned":            {sign(example, named, all), "alice@example.com"},
		"another domain":     {sign(other, testMail, all), ""},
		"unsigned":           {[]byte(testMail), ""},
		"From not signed":    {signWithoutFrom(t, example, testMail), ""},
		"tampered":           {bytes.Replace(sign(example, testMail, all), []byte("perfectly"), []byte("perfidiously"), 1), ""},
		"second From on top": {append([]byte("From: mallory@example.com\r\n"), sign(example, testMail, all)...), ""},
		"group From":         {sign(example, strings.Replace(testMail, "From: alice@example.com", "From: alice@example.com, eve@example.com", 1), all), ""},
		"subdomain signer":   {sign(example, strings.Replace(testMail, "alice@example.com", "alice@mail.example.com", 1), all), ""},
	} {
		if got := DKIMAlignedFrom(tc.raw, lookup); got != tc.want {
			t.Errorf("%s: DKIMAlignedFrom = %q, want %q", name, got, tc.want)
		}
	}
}

// signWithoutFrom hand-signs mail (simple/simple, h=to:subject:date), which
// go-msgauth's signer refuses to produce: a signature that leaves From out.
func signWithoutFrom(t *testing.T, f *dkimTestFixture, mail string) []byte {
	head, body, _ := strings.Cut(mail, "\r\n\r\n")
	bh := sha256.Sum256([]byte(body))
	var signed strings.Builder
	for _, line := range strings.Split(head, "\r\n") {
		for _, name := range []string{"To:", "Subject:", "Date:"} {
			if strings.HasPrefix(line, name) {
				signed.WriteString(line + "\r\n")
			}
		}
	}
	sig := "DKIM-Signature: v=1; a=rsa-sha256; c=simple/simple; d=" + f.domain + "; s=" + f.selector +
		"; h=to:subject:date; bh=" + base64.StdEncoding.EncodeToString(bh[:]) + "; b="
	signed.WriteString(sig)
	sum := sha256.Sum256([]byte(signed.String()))
	b, err := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return []byte(sig + base64.StdEncoding.EncodeToString(b) + "\r\n" + mail)
}
