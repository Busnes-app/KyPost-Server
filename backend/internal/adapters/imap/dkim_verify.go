package imap

import (
	"bufio"
	"bytes"
	"net/textproto"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// maxDKIMVerifications caps signatures verified per message, bounding DNS
// queries and verifier goroutines for hostile mail.
const maxDKIMVerifications = 3

// VerifyDKIMCoversHeader reports whether raw carries a valid DKIM signature
// for domain that actually covers header — that is, the header is named in the
// signature's h= tag and appears exactly once in the message.
//
// A domain-only DKIM pass proves the headers the signature *covered* are intact;
// it says nothing about a header the signer never included. Per RFC 6376 the
// verifier hashes only the fields named in h=, selects the LAST occurrence of
// each, and tolerates additional fields — so an attacker holding any genuinely
// signed message from the domain can:
//
//   - rewrite a header the signer left out of h= (common for Subject), or
//   - prepend a second copy above the signed one, which the verifier ignores
//     and every other reader (IMAP SEARCH included) sees first,
//
// and the signature still verifies. Two call sites trusted exactly that:
// send-as verification located a challenge code by Subject, and Autocrypt
// harvest read a key out of an Autocrypt header, each gated on a d= match
// alone. Both were forgeable by replaying someone else's signed mail.
//
// The duplicate check is deliberately "exactly once" rather than "the last
// one": if the header appears twice, which copy a given reader honors is not
// something this function can decide for it, so it refuses.
func VerifyDKIMCoversHeader(raw []byte, domain, header string) bool {
	return verifyDKIMCoversHeaderWithLookup(raw, domain, header, nil)
}

func verifyDKIMCoversHeaderWithLookup(raw []byte, domain, header string, lookupTXT func(string) ([]string, error)) bool {
	_, ok := DKIMSignedHeadersWithLookup(raw, domain, lookupTXT)[strings.ToLower(strings.TrimSpace(header))]
	return ok
}

// DKIMSignedHeaders returns, by lowercased name, the value of each header that
// a valid DKIM signature for domain covers and that occurs exactly once, so
// the value returned is the instance that was signed. Empty when no signature
// for domain verifies. Read values from here, not from net/mail: names are
// matched as the verifier matches them, so "Subject :" is a second Subject.
func DKIMSignedHeaders(raw []byte, domain string) map[string]string {
	return DKIMSignedHeadersWithLookup(raw, domain, nil)
}

// DKIMSignedHeadersWithLookup is DKIMSignedHeaders with the DNS TXT lookup
// injected, for tests; nil uses DNS.
func DKIMSignedHeadersWithLookup(raw []byte, domain string, lookupTXT func(string) ([]string, error)) map[string]string {
	out := map[string]string{}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || len(raw) == 0 {
		return out
	}
	fields, err := dkimHeaderFields(raw)
	if err != nil {
		return out
	}
	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT:        lookupTXT,
		MaxVerifications: maxDKIMVerifications,
	})
	if err != nil {
		return out
	}
	for _, v := range verifications {
		if v.Err != nil || !strings.EqualFold(strings.TrimSpace(v.Domain), domain) {
			continue
		}
		for _, signed := range v.HeaderKeys {
			name := strings.ToLower(strings.TrimSpace(signed))
			if vals := fields[name]; len(vals) == 1 {
				out[name] = vals[0]
			}
		}
	}
	return out
}

// dkimHeaderFields splits raw's header block the way go-msgauth's verifier
// does (dkim/header.go readHeader, parseHeaderField): continuation lines join
// the field above, and names are whitespace-trimmed. Keys are lowercased;
// values are unfolded and trimmed.
func dkimHeaderFields(raw []byte) (map[string][]string, error) {
	tr := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	var lines []string
	for {
		l, err := tr.ReadLine()
		if err != nil {
			return nil, err
		}
		if l == "" {
			break
		}
		if len(lines) > 0 && (l[0] == ' ' || l[0] == '\t') {
			lines[len(lines)-1] += l
			continue
		}
		lines = append(lines, l)
	}
	fields := map[string][]string{}
	for _, l := range lines {
		k, v, _ := strings.Cut(l, ":")
		k = strings.ToLower(strings.TrimSpace(k))
		fields[k] = append(fields[k], strings.TrimSpace(v))
	}
	return fields, nil
}
