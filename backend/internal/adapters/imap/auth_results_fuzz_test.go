package imap

import (
	"strings"
	"testing"

	goimap "github.com/BrianLeishman/go-imap"
)

// FuzzParseHeaderFieldsRecords feeds a server's UID FETCH BODY[HEADER.FIELDS]
// response, which carries sender-controlled header text, through go-imap's
// tokenizer and parseHeaderFieldsRecords. Every returned header must be one
// unfolded logical line.
func FuzzParseHeaderFieldsRecords(f *testing.F) {
	for _, s := range []string{
		fetchLine(100, arItem, "Authentication-Results: mx.example.net; dkim=pass header.d=example.com\r\n\r\n"),
		fetchLine(200, arItem, "Authentication-Results: mx.google.com;\r\n\tdkim=pass header.d=example.com;\r\n\tspf=pass smtp.mailfrom=alice@example.com\r\n\r\n"),
		fetchLine(300, arItem, "Authentication-Results: hop2.example.net; dkim=pass header.d=a.com\r\nAuthentication-Results: hop1.example.net; spf=pass smtp.mailfrom=b@b.com\r\n\r\n"),
		"* 1 FETCH (UID 400 " + arItem + " NIL)\r\n",
		"* 3 FETCH (FLAGS (\\Seen))\r\n",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		records, err := (&goimap.Dialer{}).ParseFetchResponse(raw)
		if err != nil {
			return
		}
		got, err := parseHeaderFieldsRecords(records, []string{"Authentication-Results"})
		if err != nil {
			return
		}
		for uid, lines := range got {
			for _, l := range lines {
				if strings.ContainsAny(l, "\r\n") {
					t.Fatalf("UID %d: header not unfolded: %q", uid, l)
				}
			}
		}
	})
}
