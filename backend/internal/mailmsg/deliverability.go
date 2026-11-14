package mailmsg

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// DNSResolver is the system resolver as the deliverability check uses it;
// *net.Resolver satisfies it. Tests inject their own.
type DNSResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Package tests shorten these to exercise a slow resolver.
var (
	dnsLookupTimeout     = 5 * time.Second
	deliverabilityBudget = 30 * time.Second
)

const (
	maxPTRAddresses = 16
	maxEchoedDNS    = 512
	dnsRetry        = "DNS lookup timed out or failed; check the resolver and run the check again"
)

// DNSCheck is one result. Status is pass, warn, fail or unknown; Found is
// DNS-supplied text, truncated, for display as text only.
type DNSCheck struct {
	Check   string `json:"check"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Found   string `json:"found,omitempty"`
}

type DomainChecks struct {
	Domain string     `json:"domain"`
	Checks []DNSCheck `json:"checks"`
}

// Deliverability reports DNS for a direct profile. SPF and PTR use the EHLO
// host's own addresses, not an observed egress IP, and no mail is sent.
type Deliverability struct {
	Host           string         `json:"host"`
	Domains        []DomainChecks `json:"domains"`
	PTR            DNSCheck       `json:"ptr"`
	EgressObserved bool           `json:"egressObserved"`
	MailSent       bool           `json:"mailSent"`
}

type dnsQuery struct {
	ctx context.Context
	r   DNSResolver
}

func (q dnsQuery) bounded() (context.Context, context.CancelFunc) {
	return context.WithTimeout(q.ctx, dnsLookupTimeout)
}

// Absolute names: never let a resolver search list rewrite them.
func (q dnsQuery) txt(name string) ([]string, error) {
	ctx, cancel := q.bounded()
	defer cancel()
	return q.r.LookupTXT(ctx, name+".")
}

func (q dnsQuery) ip(host string) ([]net.IP, error) {
	ctx, cancel := q.bounded()
	defer cancel()
	return q.r.LookupIP(ctx, "ip", host+".")
}

func (q dnsQuery) addr(ip net.IP) ([]string, error) {
	ctx, cancel := q.bounded()
	defer cancel()
	return q.r.LookupAddr(ctx, ip.String())
}

// absent is a definite "no such record"; any other error is unknown.
func absent(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsNotFound
}

// ipList names at most four addresses, so a large answer cannot bloat a message.
func ipList(ips []string) string {
	if len(ips) > 4 {
		return fmt.Sprintf("%s and %d more", strings.Join(ips[:4], ", "), len(ips)-4)
	}
	return strings.Join(ips, ", ")
}

func echo(s string) string {
	if len(s) > maxEchoedDNS {
		s = s[:maxEchoedDNS]
	}
	return strings.ToValidUTF8(s, "")
}

// CheckDeliverability looks up the DKIM, SPF and DMARC records of every
// sending domain and reverse DNS for the EHLO host. It only reads DNS for the
// validated profile names, signs one synthetic message locally and sends none.
func (c DomainRelay) CheckDeliverability(ctx context.Context, r DNSResolver) (Deliverability, error) {
	if c.Mode != "direct" || c.Validate() != nil {
		return Deliverability{}, ErrDomainRelay
	}
	ctx, cancel := context.WithTimeout(ctx, deliverabilityBudget)
	defer cancel()
	q := dnsQuery{ctx: ctx, r: r}
	addrs, addrErr := q.ip(c.Host)
	out := Deliverability{Host: c.Host, Domains: []DomainChecks{}}
	for _, domain := range c.Domains {
		out.Domains = append(out.Domains, DomainChecks{Domain: domain, Checks: []DNSCheck{
			c.checkDKIM(q, domain), checkSPF(q, domain, c.Host, addrs, addrErr), checkDMARC(q, domain),
		}})
	}
	out.PTR = checkPTR(q, c.Host, addrs, addrErr)
	return out, nil
}

// dkimTags parses a DKIM key record strictly: tag=value pairs, no duplicates.
func dkimTags(record string) (map[string]string, bool) {
	tags := map[string]string{}
	for _, part := range strings.Split(record, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if _, dup := tags[k]; !ok || k == "" || dup {
			return nil, false
		}
		tags[k] = strings.TrimSpace(v)
	}
	return tags, true
}

func (c DomainRelay) checkDKIM(q dnsQuery, domain string) DNSCheck {
	ch := DNSCheck{Check: "dkim"}
	k, ok := c.dkimKey(domain)
	want, err := k.publicRecord()
	if !ok || err != nil {
		ch.Name, ch.Status, ch.Message = domain, "fail", "KyPost has no usable DKIM key for "+domain+", so direct mail from it is refused; restart KyPost to create a key"
		return ch
	}
	ch.Name = k.recordName()
	publish := "publish TXT " + ch.Name + " with the value \"" + want + "\""
	txts, err := q.txt(ch.Name)
	switch {
	case absent(err) || err == nil && len(txts) == 0:
		ch.Status, ch.Message = "fail", "No DKIM record found; "+publish
		return ch
	case err != nil:
		ch.Status, ch.Message = "unknown", dnsRetry
		return ch
	case len(txts) > 1:
		ch.Status, ch.Message, ch.Found = "fail", fmt.Sprintf("%d TXT records found where receivers need exactly one; %s", len(txts), publish), echo(txts[0])
		return ch
	}
	ch.Found = echo(txts[0])
	tags, ok := dkimTags(txts[0])
	var published *rsa.PublicKey
	if ok && (tags["v"] == "" || tags["v"] == "DKIM1") && (tags["k"] == "" || tags["k"] == "rsa") {
		if der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(tags["p"]), "")); err == nil {
			parsed, _ := x509.ParsePKIXPublicKey(der)
			published, _ = parsed.(*rsa.PublicKey)
		}
	}
	key, _ := k.signer()
	switch {
	case published == nil:
		ch.Status, ch.Message = "fail", "The DKIM record cannot be parsed; "+publish
	case !published.Equal(&key.PublicKey):
		ch.Status, ch.Message = "fail", "The published DKIM key is not KyPost's key; "+publish
	case !c.dkimSelfTest(domain, ch.Name, txts[0]):
		ch.Status, ch.Message = "fail", "A message signed by KyPost does not verify against the published record; "+publish
	case slices.Contains(strings.Split(strings.ReplaceAll(tags["t"], " ", ""), ":"), "y"):
		ch.Status, ch.Message = "warn", "The DKIM record is in testing mode (t=y), so receivers may ignore failures; remove t=y"
	default:
		ch.Status, ch.Message = "pass", "DKIM record matches KyPost's key and verifies a locally signed test message"
	}
	return ch
}

// dkimSelfTest signs a synthetic message with the stored key and verifies it
// against the fetched record. Nothing is sent.
func (c DomainRelay) dkimSelfTest(domain, name, record string) bool {
	msg := "From: dkim-check@" + domain + "\r\nTo: dkim-check@" + domain + "\r\nSubject: KyPost DKIM check\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\nMessage-ID: <dkim-check@" + domain + ">\r\n\r\nKyPost DKIM self-test. Never sent.\r\n"
	signed, err := c.dkimSign(domain, []byte(msg))
	if err != nil {
		return false
	}
	result, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{LookupTXT: func(n string) ([]string, error) {
		if n != name {
			return nil, &net.DNSError{IsNotFound: true}
		}
		return []string{record}, nil
	}})
	return err == nil && len(result) == 1 && result[0].Err == nil && result[0].Domain == domain
}

func isSPF(record string) bool {
	return strings.EqualFold(record, "v=spf1") || len(record) > 6 && strings.EqualFold(record[:7], "v=spf1 ")
}

func checkSPF(q dnsQuery, domain, host string, addrs []net.IP, addrErr error) DNSCheck {
	ch := DNSCheck{Check: "spf", Name: domain}
	publish := "publish TXT " + domain + " with the value \"" + recommendedSPF(host) + "\""
	txts, err := q.txt(domain)
	if err != nil && !absent(err) {
		ch.Status, ch.Message = "unknown", dnsRetry
		return ch
	}
	var records []string
	for _, t := range txts {
		if isSPF(t) {
			records = append(records, t)
		}
	}
	switch {
	case len(records) == 0:
		ch.Status, ch.Message = "fail", "No SPF record found; "+publish
		return ch
	case len(records) > 1:
		ch.Status, ch.Message, ch.Found = "fail", fmt.Sprintf("%d SPF records found; receivers treat more than one as an error. Keep one, for example: %s", len(records), recommendedSPF(host)), echo(records[0])
		return ch
	}
	ch.Found = echo(records[0])
	switch {
	case absent(addrErr) || addrErr == nil && len(addrs) == 0:
		ch.Status, ch.Message = "fail", host+" has no A or AAAA address, so SPF cannot authorize it; publish its address records"
		return ch
	case addrErr != nil:
		ch.Status, ch.Message = "unknown", dnsRetry
		return ch
	}
	ch.Status, ch.Message = spfVerdict(records[0], host, addrs)
	if ch.Status == "fail" {
		ch.Message += "; " + publish
	}
	return ch
}

// spfVerdict evaluates only ip4, ip6, a:<host> and all, per address and in
// order. Anything needing more DNS (include, redirect, exists, mx, ptr, other
// a forms, macros) makes the result unknown, never pass.
func spfVerdict(record, host string, addrs []net.IP) (string, string) {
	terms := strings.Fields(strings.ToLower(record))[1:]
	redirect := false
	for _, t := range terms {
		if t == "all" || t == "+all" {
			return "fail", "The SPF record ends in +all, which authorizes every server on the Internet"
		}
		redirect = redirect || strings.HasPrefix(t, "redirect=")
	}
	unknown, refused := false, []string{}
	for _, ip := range addrs {
		result := "?" // neutral when nothing matches
	evaluate:
		for _, t := range terms {
			qualifier := byte('+')
			if strings.ContainsRune("+-~?", rune(t[0])) {
				qualifier, t = t[0], t[1:]
			}
			mechanism, value, _ := strings.Cut(t, ":")
			switch {
			case strings.Contains(mechanism, "="): // a modifier; redirect is handled below
			case t == "all":
				result = string(qualifier)
				break evaluate
			case mechanism == "a" && strings.TrimSuffix(value, ".") == host:
				result = string(qualifier)
				break evaluate
			case mechanism == "ip4" || mechanism == "ip6":
				if !strings.Contains(value, "/") {
					value += map[bool]string{true: "/32", false: "/128"}[mechanism == "ip4"]
				}
				_, network, err := net.ParseCIDR(value)
				if err != nil || (network.IP.To4() != nil) != (mechanism == "ip4") {
					return "fail", "The SPF record has an invalid " + mechanism + " mechanism"
				}
				// A range this wide authorizes much of the Internet, as +all does.
				if ones, _ := network.Mask.Size(); qualifier == '+' && ones < map[bool]int{true: 8, false: 32}[mechanism == "ip4"] {
					return "fail", "The SPF record's " + mechanism + " range authorizes far more servers than this one; list only " + host + "'s addresses"
				}
				if network.Contains(ip) {
					result = string(qualifier)
					break evaluate
				}
			default:
				result = "unknown"
				break evaluate
			}
		}
		switch {
		case result == "unknown" || result == "?" && redirect:
			unknown = true
		case result != "+":
			refused = append(refused, ip.String())
		}
	}
	switch {
	case len(refused) > 0:
		return "fail", "SPF does not authorize " + ipList(refused) + " (an address of " + host + ") before its all mechanism"
	case unknown:
		return "unknown", "SPF uses include, redirect, exists or other mechanisms KyPost does not evaluate; confirm it authorizes " + host + "'s addresses"
	}
	return "pass", "SPF authorizes every address of " + host + " (KyPost cannot observe its real egress address)"
}

func dmarcRecords(q dnsQuery, domain string) ([]string, error) {
	txts, err := q.txt("_dmarc." + domain)
	if err != nil && !absent(err) {
		return nil, err
	}
	var records []string
	for _, t := range txts {
		if t == "v=DMARC1" || strings.HasPrefix(t, "v=DMARC1;") || strings.HasPrefix(t, "v=DMARC1 ") {
			records = append(records, t)
		}
	}
	return records, nil
}

func checkDMARC(q dnsQuery, domain string) DNSCheck {
	ch := DNSCheck{Check: "dmarc", Name: "_dmarc." + domain}
	publish := "publish TXT _dmarc." + domain + " with the value \"" + recommendedDMARC + "\""
	records, err := dmarcRecords(q, domain)
	inherited := false
	if org, e := publicsuffix.EffectiveTLDPlusOne(domain); err == nil && len(records) == 0 && e == nil && org != domain {
		ch.Name, inherited = "_dmarc."+org, true
		records, err = dmarcRecords(q, org)
	}
	switch {
	case err != nil:
		ch.Status, ch.Message = "unknown", dnsRetry
		return ch
	case len(records) == 0:
		ch.Status, ch.Message = "fail", "No DMARC record found; "+publish
		return ch
	case len(records) > 1:
		ch.Status, ch.Message, ch.Found = "fail", fmt.Sprintf("%d DMARC records found at %s; receivers ignore DMARC unless there is exactly one", len(records), ch.Name), echo(records[0])
		return ch
	}
	ch.Found = echo(records[0])
	record, err := dmarc.Parse(records[0])
	if err != nil {
		ch.Status, ch.Message = "fail", "The DMARC record cannot be parsed; "+publish
		return ch
	}
	policy := record.Policy
	if inherited && record.SubdomainPolicy != "" {
		policy = record.SubdomainPolicy
	}
	switch {
	case policy == dmarc.PolicyNone:
		ch.Status, ch.Message = "warn", "DMARC policy is none, so receivers still accept mail forged as "+domain+"; use p=quarantine or p=reject"
	case record.Percent != nil && *record.Percent < 100:
		ch.Status, ch.Message = "warn", fmt.Sprintf("DMARC applies to only %d%% of failing mail; remove pct or set pct=100", *record.Percent)
	default:
		ch.Status, ch.Message = "pass", "DMARC policy is "+string(policy)
	}
	return ch
}

func checkPTR(q dnsQuery, host string, addrs []net.IP, addrErr error) DNSCheck {
	ch := DNSCheck{Check: "ptr", Name: host}
	switch {
	case absent(addrErr) || addrErr == nil && len(addrs) == 0:
		ch.Status, ch.Message = "fail", host+" has no A or AAAA address; publish one and set its reverse DNS (PTR) to "+host
		return ch
	case addrErr != nil:
		ch.Status, ch.Message = "unknown", dnsRetry
		return ch
	}
	checked := addrs[:min(len(addrs), maxPTRAddresses)]
	var missing []string
	for _, ip := range checked {
		names, err := q.addr(ip)
		if err != nil && !absent(err) {
			ch.Status, ch.Message = "unknown", dnsRetry
			return ch
		}
		// The address came from host's own lookup, so a matching PTR is forward-confirmed.
		if !slices.ContainsFunc(names, func(n string) bool { return strings.TrimSuffix(strings.ToLower(n), ".") == host }) {
			missing = append(missing, ip.String())
		}
	}
	switch {
	case len(missing) == len(checked):
		ch.Status, ch.Message = "fail", "No address of "+host+" has reverse DNS (PTR) back to "+host+"; ask your hosting provider to set it"
	case len(missing) > 0:
		ch.Status, ch.Message = "warn", "Reverse DNS (PTR) of "+ipList(missing)+" does not name "+host+"; set it with your hosting provider"
	case len(addrs) > len(checked):
		ch.Status, ch.Message = "warn", fmt.Sprintf("The first %d addresses of %s have matching reverse DNS; the other %d were not checked", len(checked), host, len(addrs)-len(checked))
	default:
		ch.Status, ch.Message = "pass", "Every address of "+host+" has reverse DNS (PTR) back to "+host
	}
	return ch
}
