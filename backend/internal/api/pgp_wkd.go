package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// zBase32 is the alphabet from the Z-Base-32 encoding used by WKD.
const zBase32 = "ybndrfg8ejkmcpqxot1uwisza345h769"

// wkdHashLocalPart returns the WKD "hashed local-part": the lowercased
// local-part hashed with SHA-1 and encoded with Z-Base-32 (no padding).
func wkdHashLocalPart(localPart string) string {
	sum := sha1.Sum([]byte(strings.ToLower(localPart)))
	var b strings.Builder
	bits := 0
	var acc uint32
	for _, c := range sum {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			b.WriteByte(zBase32[(acc>>uint(bits))&0x1f])
		}
	}
	if bits > 0 {
		b.WriteByte(zBase32[(acc<<uint(5-bits))&0x1f])
	}
	return b.String()
}

// validateDiscoveredKey parses an armored public key obtained from an
// untrusted discovery source and confirms it is safe to auto-use for email:
// it must be usable (not revoked/expired) and actually carry email as a UID.
func validateDiscoveredKey(armored, email string) (string, error) {
	key, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return "", fmt.Errorf("parse discovered key: %w", err)
	}
	if err := pgpmail.CheckArmoredKeyPolicy(armored); err != nil {
		return "", err
	}
	status, err := pgpmail.CheckKeyStatus(armored)
	if err != nil {
		return "", err
	}
	if !status.Usable() {
		return "", fmt.Errorf("discovered key for %s is revoked or expired", email)
	}
	target := strings.ToLower(strings.TrimSpace(email))
	entity := key.GetEntity()
	if entity == nil {
		return "", fmt.Errorf("discovered key has no entity")
	}
	for _, uid := range entity.Identities {
		if strings.ToLower(strings.TrimSpace(uid.UserId.Email)) == target {
			return key.GetFingerprint(), nil
		}
	}
	return "", fmt.Errorf("discovered key does not carry %s as a user ID", email)
}

// wkdBaseURLOverride, when set (tests only), replaces the derived
// scheme+host so lookups hit an httptest.Server. Mirrors keyserverBaseURL.
var wkdBaseURLOverride string

// validWKDDomain reports whether domain is a bare DNS hostname, and so is safe
// to concatenate into a URL.
//
// The domain reaches us from a recipient address the user typed, and
// mail.ParseAddress is much more permissive than DNS: '/', '?', '#' and '@'
// are all valid atext, so "a@evil.com/admin" is an address as far as the mail
// parser is concerned. Concatenated verbatim it yields
// https://evil.com/admin/.well-known/..., which hands the user the path of a
// request this server makes. The SSRF guard cannot catch that — the host is
// genuinely the public host it looks like; it is the rest of the URL that has
// been rewritten — and by the time the string has been through url.Parse an
// injected path is indistinguishable from a real one. So it has to be caught
// here, before any URL exists.
//
// Deliberately syntax only. Whether a syntactically fine hostname is one we
// may actually talk to (localhost, 169.254.169.254, anything resolving into
// private space) is validateOutboundURL and netguard.DialContext's decision,
// and duplicating it here would mean two rules to keep in agreement.
func validWKDDomain(domain string) bool {
	// 253 is the longest name representable in a 255-byte wire-format QNAME.
	if domain == "" || len(domain) > 253 {
		return false
	}
	// A single label is a valid hostname but never a valid mail domain, and
	// accepting one lets a bare token like "ev" (from "a@ev/il.com", whose
	// host truncates at the slash) be resolved through the container's DNS
	// search domain into an internal name.
	if !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
				c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// wkdCandidateURLs returns the advanced-method URL first, then the
// direct-method URL, for the given local-part/domain. It returns no candidates
// at all for a domain that is not a plain hostname — see validWKDDomain.
func wkdCandidateURLs(localPart, domain string) []string {
	hu := wkdHashLocalPart(localPart)
	l := url.QueryEscape(localPart)
	if wkdBaseURLOverride == "" && !validWKDDomain(domain) {
		return nil
	}
	if wkdBaseURLOverride != "" {
		// Tests: single host serves the direct-method path.
		return []string{
			wkdBaseURLOverride + "/.well-known/openpgpkey/hu/" + hu + "?l=" + l,
		}
	}
	return []string{
		"https://openpgpkey." + domain + "/.well-known/openpgpkey/" + domain + "/hu/" + hu + "?l=" + l,
		"https://" + domain + "/.well-known/openpgpkey/hu/" + hu + "?l=" + l,
	}
}

// maxWKDCertificates bounds how many certificates of one WKD answer are
// validated: each costs a parse and a status check, and the 1 MiB body cap
// alone would admit hundreds.
const maxWKDCertificates = 8

// wkdCert is one validated certificate from a WKD answer.
type wkdCert struct {
	Armored     string
	Fingerprint string
	Class       keyClass
}

// fetchWKDKeys attempts Web Key Directory discovery for email, trying the
// advanced method then the direct method. It returns every certificate in the
// first answer that has any valid one, each validated to carry email as a UID
// and to be currently usable, best first (keyClass.rank).
func fetchWKDKeys(ctx context.Context, email string) ([]wkdCert, error) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return nil, fmt.Errorf("invalid email %q", email)
	}
	localPart, domain := email[:at], email[at+1:]
	client := newSSRFSafeHTTPClient(10 * time.Second)

	candidates := wkdCandidateURLs(localPart, domain)
	if len(candidates) == 0 {
		// Distinguished from "no key published" on purpose: this address can
		// never be looked up, so a caller retrying or caching a negative
		// result is answering a different question.
		return nil, fmt.Errorf("invalid WKD domain %q", domain)
	}

	var lastErr error
	for _, u := range candidates {
		allowedSchemes := []string{"https"}
		if wkdBaseURLOverride != "" {
			allowedSchemes = []string{"http", "https"}
		}
		if err := validateOutboundURL(u, allowedSchemes...); err != nil {
			lastErr = fmt.Errorf("unsafe WKD URL: %w", err)
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || readErr != nil {
			lastErr = fmt.Errorf("wkd %s: status %d", u, resp.StatusCode)
			continue
		}
		certs, err := wkdCertificates(body, email)
		if err != nil {
			lastErr = fmt.Errorf("wkd %s: %w", u, err)
			continue
		}
		return certs, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no WKD key for %s", email)
	}
	return nil, lastErr
}

// wkdCertificates reads every certificate in a WKD answer, which may hold
// several concatenated (draft-koch-openpgp-webkey-service-22 §3.1). Reading
// only the first let the server's ordering, not KyPost, decide which one we
// use. Each must pass validateDiscoveredKey on its own; the rest are dropped.
func wkdCertificates(body []byte, email string) ([]wkdCert, error) {
	read := openpgp.ReadKeyRing // WKD serves binary keys
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN")) {
		read = openpgp.ReadArmoredKeyRing
	}
	entities, err := read(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var out []wkdCert
	for i, e := range entities {
		if i == maxWKDCertificates {
			break
		}
		key, kerr := crypto.NewKeyFromEntity(e)
		if kerr != nil {
			err = kerr
			continue
		}
		armored, kerr := key.GetArmoredPublicKey()
		if kerr != nil {
			err = kerr
			continue
		}
		fp, kerr := validateDiscoveredKey(armored, email)
		if kerr != nil {
			err = kerr
			continue
		}
		out = append(out, wkdCert{Armored: armored, Fingerprint: fp, Class: classOfEntity(e)})
	}
	if len(out) == 0 {
		if err == nil {
			err = fmt.Errorf("no certificate for %s", email)
		}
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Class.rank() > out[j].Class.rank() })
	return out, nil
}
