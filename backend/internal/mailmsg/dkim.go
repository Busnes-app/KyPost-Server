package mailmsg

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIM key states. Only the active key signs; next waits for its record to
// be confirmed live; retiring stays published for mail already sent; a
// removable entry has lost its private key and only names a record to delete.
const (
	DKIMActive    = "active"
	DKIMNext      = "next"
	DKIMRetiring  = "retiring"
	DKIMRemovable = "removable"
)

// DKIMKey is one selector of a domain. It is stored only inside the sealed
// relay file; the API shows the public record, never the private key.
type DKIMKey struct {
	Domain             string    `json:"domain"`
	Selector           string    `json:"selector"`
	State              string    `json:"state"`
	Private            []byte    `json:"privateKey,omitempty"` // PKCS #8 DER, RSA-2048
	CreatedAt          time.Time `json:"createdAt"`
	ActivatedAt        time.Time `json:"activatedAt,omitzero"`
	RetiredAt          time.Time `json:"retiredAt,omitzero"`
	KeepPublishedUntil time.Time `json:"keepPublishedUntil,omitzero"`
}

func (a DKIMKey) equal(b DKIMKey) bool {
	return a.Domain == b.Domain && a.Selector == b.Selector && a.State == b.State && bytes.Equal(a.Private, b.Private) &&
		a.CreatedAt.Equal(b.CreatedAt) && a.ActivatedAt.Equal(b.ActivatedAt) && a.RetiredAt.Equal(b.RetiredAt) && a.KeepPublishedUntil.Equal(b.KeepPublishedUntil)
}

// DNSRecord is a record the administrator publishes. Kind is "spf" or
// "dmarc" for recommended records and empty for a DKIM key, which also
// carries its rotation state. A removable key has no RecordValue.
type DNSRecord struct {
	Domain    string    `json:"domain"`
	Kind      string    `json:"kind,omitempty"`
	Selector  string    `json:"selector,omitempty"`
	State     string    `json:"state,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	AgeDays   *int      `json:"ageDays,omitempty"`
	// RotationRecommendedAt is 180 days after creation; KyPost never rotates on its own.
	RotationRecommendedAt time.Time `json:"rotationRecommendedAt,omitzero"`
	RotationRecommended   *bool     `json:"rotationRecommended,omitempty"`
	RetiredAt             time.Time `json:"retiredAt,omitzero"`
	KeepPublishedUntil    time.Time `json:"keepPublishedUntil,omitzero"`
	RecordName            string    `json:"recordName"`
	RecordType            string    `json:"recordType"`
	RecordValue           string    `json:"recordValue,omitempty"`
}

var dkimSelector = regexp.MustCompile(`^kypost-[0-9a-f]{8}$`)

// Signed when present (RFC 6376 section 5.4.1). Bcc never reaches the wire.
var dkimSignedHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"}

// at is the stored form of a time: UTC, whole seconds, so re-sealing is stable.
func at(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// newDKIMKey makes an RSA-2048 key with a selector unused on c's domain.
func (c DomainRelay) newDKIMKey(domain, state string, now time.Time) (DKIMKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return DKIMKey{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return DKIMKey{}, err
	}
	for {
		id := make([]byte, 4)
		if _, err := rand.Read(id); err != nil {
			return DKIMKey{}, err
		}
		selector := "kypost-" + hex.EncodeToString(id)
		if !slices.ContainsFunc(c.DKIM, func(k DKIMKey) bool { return k.Domain == domain && k.Selector == selector }) {
			k := DKIMKey{Domain: domain, Selector: selector, State: state, Private: der, CreatedAt: at(now)}
			if state == DKIMActive {
				k.ActivatedAt = k.CreatedAt
			}
			return k, nil
		}
	}
}

func (k DKIMKey) signer() (*rsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(k.Private)
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok || key.N.BitLen() != 2048 {
		return nil, errors.New("invalid DKIM key")
	}
	return key, nil
}

// publicRecord is the TXT value receivers fetch: the key's SPKI in p=.
func (k DKIMKey) publicRecord() (string, error) {
	key, err := k.signer()
	if err != nil {
		return "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(spki), nil
}

func (k DKIMKey) recordName() string { return k.Selector + "._domainkey." + k.Domain }

var dkimStateRank = map[string]int{DKIMActive: 0, DKIMNext: 1, DKIMRetiring: 2, DKIMRemovable: 3}

// compareDKIM is the stored order: domain, state, then selector.
func compareDKIM(a, b DKIMKey) int {
	if d := strings.Compare(a.Domain, b.Domain); d != 0 {
		return d
	}
	if d := dkimStateRank[a.State] - dkimStateRank[b.State]; d != 0 {
		return d
	}
	return strings.Compare(a.Selector, b.Selector)
}

// validDKIMKeys checks shape only; signing parses the key and refuses (451)
// rather than letting one bad key make the whole profile unreadable. Per
// domain: at most one active and one next (a next needs an active), at most
// four retiring or removable, unique selectors, and no private key left in a
// removable entry.
func validDKIMKeys(keys []DKIMKey, known func(string) bool) bool {
	if !slices.IsSortedFunc(keys, compareDKIM) {
		return false
	}
	type tally struct{ active, next, old int }
	counts := map[string]*tally{}
	selectors := map[string]bool{}
	for _, k := range keys {
		t := counts[k.Domain]
		if t == nil {
			t = &tally{}
			counts[k.Domain] = t
		}
		if !known(k.Domain) || !dkimSelector.MatchString(k.Selector) || selectors[k.Domain+" "+k.Selector] || k.CreatedAt.IsZero() || len(k.Private) > 4096 {
			return false
		}
		selectors[k.Domain+" "+k.Selector] = true
		retired := !k.RetiredAt.IsZero() && !k.KeepPublishedUntil.IsZero()
		switch k.State {
		case DKIMActive:
			t.active++
			if len(k.Private) == 0 || !k.RetiredAt.IsZero() {
				return false
			}
		case DKIMNext:
			t.next++
			if len(k.Private) == 0 || !k.ActivatedAt.IsZero() || !k.RetiredAt.IsZero() {
				return false
			}
		case DKIMRetiring:
			t.old++
			if len(k.Private) == 0 || !retired {
				return false
			}
		case DKIMRemovable:
			t.old++
			if len(k.Private) != 0 || !retired {
				return false
			}
		default:
			return false
		}
	}
	for _, t := range counts {
		if t.active > 1 || t.next > 1 || t.old > 4 || t.next == 1 && t.active == 0 {
			return false
		}
	}
	return true
}

// dkimKey returns domain's key in state.
func (c DomainRelay) dkimKey(domain, state string) (DKIMKey, bool) {
	i := slices.IndexFunc(c.DKIM, func(k DKIMKey) bool { return k.Domain == domain && k.State == state })
	if i < 0 {
		return DKIMKey{}, false
	}
	return c.DKIM[i], true
}

// keepDKIMKeys keeps prior keys while their domain is in Domains ∪
// RetiredDomains, so keys outlive mode switches, and in direct mode gives
// every sending domain without an active key its first one. A first key
// signs at once: no confirmed key exists for the domain yet.
func (c *DomainRelay) keepDKIMKeys(prior []DKIMKey, now time.Time) error {
	c.DKIM = nil
	for _, k := range prior {
		if c.Historical(k.Domain) {
			c.DKIM = append(c.DKIM, k)
		}
	}
	if c.Mode == "direct" {
		for _, domain := range c.Domains {
			if _, ok := c.dkimKey(domain, DKIMActive); ok {
				continue
			}
			k, err := c.newDKIMKey(domain, DKIMActive, now)
			if err != nil {
				return ErrDomainRelay
			}
			c.DKIM = append(c.DKIM, k)
		}
	}
	slices.SortFunc(c.DKIM, compareDKIM)
	return nil
}

// dkimSign prepends one DKIM-Signature by domain's active key. The outbox has
// already bound the header From to the envelope sender, so d= is the From domain.
func (c DomainRelay) dkimSign(domain string, msg []byte) ([]byte, error) {
	k, ok := c.dkimKey(domain, DKIMActive)
	if !ok {
		return nil, errors.New("no DKIM key for the From domain")
	}
	return k.sign(msg)
}

// sign leaves the message bytes after the new signature field unchanged.
func (k DKIMKey) sign(msg []byte) ([]byte, error) {
	key, err := k.signer()
	if err != nil {
		return nil, err
	}
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(msg))).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	keys := []string{"From"} // one more From than present: oversigned
	for _, name := range dkimSignedHeaders {
		for range header.Values(name) {
			keys = append(keys, name)
		}
	}
	var out bytes.Buffer
	err = dkim.Sign(&out, bytes.NewReader(msg), &dkim.SignOptions{
		Domain: k.Domain, Selector: k.Selector, Signer: key, Hash: crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: keys,
	})
	return out.Bytes(), err
}

// DKIMRecords lists every key of each sending domain with its rotation state
// in direct mode, and nothing in relay mode, where the provider signs.
func (c DomainRelay) DKIMRecords(now time.Time) []DNSRecord {
	records := []DNSRecord{}
	if c.Mode != "direct" {
		return records
	}
	for _, k := range c.DKIM {
		if !c.Sends(k.Domain) {
			continue
		}
		r := DNSRecord{Domain: k.Domain, Selector: k.Selector, State: k.State, CreatedAt: k.CreatedAt, RecordName: k.recordName(), RecordType: "TXT"}
		switch k.State {
		case DKIMActive:
			days := max(0, int(now.Sub(k.CreatedAt)/(24*time.Hour)))
			due := !now.Before(k.CreatedAt.Add(dkimRotationAge))
			r.AgeDays, r.RotationRecommendedAt, r.RotationRecommended = &days, k.CreatedAt.Add(dkimRotationAge), &due
		case DKIMRetiring, DKIMRemovable:
			r.RetiredAt, r.KeepPublishedUntil = k.RetiredAt, k.KeepPublishedUntil
		}
		if k.State != DKIMRemovable {
			value, err := k.publicRecord()
			if err != nil {
				continue
			}
			r.RecordValue = value
		}
		records = append(records, r)
	}
	return records
}

// RecommendedRecords are the strict SPF and DMARC records for direct mode.
func (c DomainRelay) RecommendedRecords() []DNSRecord {
	records := []DNSRecord{}
	for _, domain := range c.Domains {
		records = append(records,
			DNSRecord{Domain: domain, Kind: "spf", RecordName: domain, RecordType: "TXT", RecordValue: recommendedSPF(c.Host)},
			DNSRecord{Domain: domain, Kind: "dmarc", RecordName: "_dmarc." + domain, RecordType: "TXT", RecordValue: recommendedDMARC})
	}
	return records
}

func recommendedSPF(host string) string { return "v=spf1 a:" + host + " -all" }

const recommendedDMARC = "v=DMARC1; p=quarantine; adkim=s; aspf=s"
