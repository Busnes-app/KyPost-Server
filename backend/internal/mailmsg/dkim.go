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

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMKey signs direct mail for one domain. It is stored only inside the
// sealed relay file; the API shows the public record, never this key.
type DKIMKey struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	Private  []byte `json:"privateKey"` // PKCS #8 DER, RSA-2048
}

// DNSRecord is a record the administrator publishes. Kind is "spf" or
// "dmarc" for recommended records and empty for a DKIM key record.
type DNSRecord struct {
	Domain      string `json:"domain"`
	Kind        string `json:"kind,omitempty"`
	Selector    string `json:"selector,omitempty"`
	RecordName  string `json:"recordName"`
	RecordType  string `json:"recordType"`
	RecordValue string `json:"recordValue"`
}

var dkimSelector = regexp.MustCompile(`^kypost-[0-9a-f]{8}$`)

// Signed when present (RFC 6376 section 5.4.1). Bcc never reaches the wire.
var dkimSignedHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "Reply-To", "In-Reply-To", "References", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"}

func newDKIMKey(domain string) (DKIMKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return DKIMKey{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return DKIMKey{}, err
	}
	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return DKIMKey{}, err
	}
	return DKIMKey{Domain: domain, Selector: "kypost-" + hex.EncodeToString(id), Private: der}, nil
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

// validDKIMKeys checks shape only; signing parses the key and refuses (451)
// rather than letting one bad key make the whole profile unreadable.
func validDKIMKeys(keys []DKIMKey, known func(string) bool) bool {
	for i, k := range keys {
		if i > 0 && keys[i-1].Domain >= k.Domain || !known(k.Domain) || !dkimSelector.MatchString(k.Selector) || len(k.Private) == 0 || len(k.Private) > 4096 {
			return false
		}
	}
	return true
}

func (c DomainRelay) dkimKey(domain string) (DKIMKey, bool) {
	i := slices.IndexFunc(c.DKIM, func(k DKIMKey) bool { return k.Domain == domain })
	if i < 0 {
		return DKIMKey{}, false
	}
	return c.DKIM[i], true
}

// keepDKIMKeys keeps prior keys while their domain is in Domains ∪
// RetiredDomains, so keys outlive mode switches, and in direct mode adds one
// for every sending domain without a key. Existing keys never change.
func (c *DomainRelay) keepDKIMKeys(prior []DKIMKey) error {
	c.DKIM = nil
	for _, k := range prior {
		if c.Historical(k.Domain) {
			c.DKIM = append(c.DKIM, k)
		}
	}
	if c.Mode == "direct" {
		for _, domain := range c.Domains {
			if _, ok := c.dkimKey(domain); ok {
				continue
			}
			k, err := newDKIMKey(domain)
			if err != nil {
				return ErrDomainRelay
			}
			c.DKIM = append(c.DKIM, k)
		}
	}
	slices.SortFunc(c.DKIM, func(a, b DKIMKey) int { return strings.Compare(a.Domain, b.Domain) })
	return nil
}

// dkimSign prepends one DKIM-Signature for domain to normalized MIME and
// leaves the message bytes after it unchanged. The outbox has already bound
// the header From to the envelope sender, so d= is the From domain.
func (c DomainRelay) dkimSign(domain string, msg []byte) ([]byte, error) {
	k, ok := c.dkimKey(domain)
	if !ok {
		return nil, errors.New("no DKIM key for the From domain")
	}
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
		Domain: domain, Selector: k.Selector, Signer: key, Hash: crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: keys,
	})
	return out.Bytes(), err
}

// DKIMRecords lists the public DKIM record of each sending domain in direct
// mode, and nothing in relay mode, where the provider signs.
func (c DomainRelay) DKIMRecords() []DNSRecord {
	records := []DNSRecord{}
	if c.Mode != "direct" {
		return records
	}
	for _, domain := range c.Domains {
		k, ok := c.dkimKey(domain)
		if !ok {
			continue
		}
		value, err := k.publicRecord()
		if err != nil {
			continue
		}
		records = append(records, DNSRecord{Domain: domain, Selector: k.Selector, RecordName: k.recordName(), RecordType: "TXT", RecordValue: value})
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
