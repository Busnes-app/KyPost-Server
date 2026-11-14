package mailmsg

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDNS serves fixed answers. Missing names are NXDOMAIN; errs entries
// override any name, and block waits for the lookup's deadline.
type fakeDNS struct {
	mu        sync.Mutex
	txt       map[string][]string
	ips       []net.IP
	ptr       map[string][]string
	errs      map[string]error
	block     bool
	calls     int
	addrCalls int
	deadlines []time.Duration
}

func (f *fakeDNS) lookup(ctx context.Context, name string) error {
	f.mu.Lock()
	f.calls++
	if d, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(d))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
	err, failing := f.errs[name]
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return &net.DNSError{Err: "timeout", Name: name, IsTimeout: true}
	}
	if failing {
		return err
	}
	return nil
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if err := f.lookup(ctx, name); err != nil {
		return nil, err
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (f *fakeDNS) LookupIP(ctx context.Context, _, host string) ([]net.IP, error) {
	if err := f.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(f.ips) == 0 {
		return nil, notFound(host)
	}
	return f.ips, nil
}

func (f *fakeDNS) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	f.mu.Lock()
	f.addrCalls++
	f.mu.Unlock()
	if err := f.lookup(ctx, addr); err != nil {
		return nil, err
	}
	if v, ok := f.ptr[addr]; ok {
		return v, nil
	}
	return nil, notFound(addr)
}

func deliverabilityProfile(domain string) DomainRelay {
	c := directTestProfile()
	c.Domains, c.DKIM = []string{domain}, []DKIMKey{testDKIMKey(domain)}
	return c
}

// passingDNS publishes every record KyPost recommends for domain.
func passingDNS(t *testing.T, domain string) *fakeDNS {
	t.Helper()
	k := testDKIMKey(domain)
	record, err := k.publicRecord()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeDNS{
		txt: map[string][]string{
			k.recordName() + ".":     {record},
			domain + ".":             {"google-site-verification=x", recommendedSPF("mail.example.test")},
			"_dmarc." + domain + ".": {recommendedDMARC},
		},
		ips:  []net.IP{net.ParseIP("203.0.113.5")},
		ptr:  map[string][]string{"203.0.113.5": {"mail.example.test."}},
		errs: map[string]error{},
	}
}

func checkOf(t *testing.T, d Deliverability, name string) DNSCheck {
	t.Helper()
	if name == "ptr" {
		return d.PTR
	}
	if len(d.Domains) != 1 {
		t.Fatal("want one domain", d.Domains)
	}
	for _, c := range d.Domains[0].Checks {
		if c.Check == name {
			return c
		}
	}
	t.Fatal("missing check", name)
	return DNSCheck{}
}

func TestDeliverabilityChecks(t *testing.T) {
	const domain = "example.test"
	k := testDKIMKey(domain)
	dkimName := k.recordName() + "."
	record, _ := k.publicRecord()
	other, _ := testDKIMKey("other.test").publicRecord()
	servfail := &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	spf := func(v string) func(*fakeDNS) { return func(f *fakeDNS) { f.txt[domain+"."] = []string{v} } }
	dmarcTXT := func(v ...string) func(*fakeDNS) { return func(f *fakeDNS) { f.txt["_dmarc."+domain+"."] = v } }
	for _, c := range []struct {
		name, check, want string
		domain            string
		edit              func(*fakeDNS)
	}{
		{"dkim pass", "dkim", "pass", "", nil},
		{"dkim missing", "dkim", "fail", "", func(f *fakeDNS) { delete(f.txt, dkimName) }},
		{"dkim different key", "dkim", "fail", "", func(f *fakeDNS) { f.txt[dkimName] = []string{other} }},
		{"dkim unparseable", "dkim", "fail", "", func(f *fakeDNS) { f.txt[dkimName] = []string{"v=DKIM1; k=rsa; p=@@not-base64"} }},
		{"dkim no tags", "dkim", "fail", "", func(f *fakeDNS) { f.txt[dkimName] = []string{"garbage"} }},
		{"dkim two records", "dkim", "fail", "", func(f *fakeDNS) { f.txt[dkimName] = []string{record, record} }},
		{"dkim testing", "dkim", "warn", "", func(f *fakeDNS) { f.txt[dkimName] = []string{record + "; t=y"} }},
		{"dkim servfail", "dkim", "unknown", "", func(f *fakeDNS) { f.errs[dkimName] = servfail }},
		{"spf pass a", "spf", "pass", "", nil},
		{"spf none", "spf", "fail", "", func(f *fakeDNS) { delete(f.txt, domain+".") }},
		{"spf two", "spf", "fail", "", func(f *fakeDNS) { f.txt[domain+"."] = []string{"v=spf1 -all", "v=spf1 a:mail.example.test -all"} }},
		{"spf ip4", "spf", "pass", "", spf("v=spf1 ip4:203.0.113.0/24 -all")},
		{"spf ip4 and ip6", "spf", "pass", "", func(f *fakeDNS) {
			f.ips = append(f.ips, net.ParseIP("2001:db8::5"))
			f.ptr["2001:db8::5"] = []string{"mail.example.test"}
			f.txt[domain+"."] = []string{"v=spf1 ip4:203.0.113.5 ip6:2001:db8::/32 ~all"}
		}},
		{"spf ip6 missing", "spf", "fail", "", func(f *fakeDNS) {
			f.ips = append(f.ips, net.ParseIP("2001:db8::5"))
			f.txt[domain+"."] = []string{"v=spf1 ip4:203.0.113.5 -all"}
		}},
		{"spf include", "spf", "unknown", "", spf("v=spf1 include:_spf.example.net -all")},
		{"spf redirect", "spf", "unknown", "", spf("v=spf1 redirect=_spf.example.net")},
		{"spf exists", "spf", "unknown", "", spf("v=spf1 exists:%{i}.example.net -all")},
		{"spf include after cover", "spf", "pass", "", spf("v=spf1 a:mail.example.test include:_spf.example.net -all")},
		{"spf not covered", "spf", "fail", "", spf("v=spf1 ip4:198.51.100.1 -all")},
		{"spf softfail only", "spf", "fail", "", spf("v=spf1 ~all")},
		{"spf plus all", "spf", "fail", "", spf("v=spf1 a:mail.example.test +all")},
		{"spf bare all", "spf", "fail", "", spf("v=spf1 all")},
		{"spf negative first", "spf", "fail", "", spf("v=spf1 -ip4:203.0.113.5 a:mail.example.test -all")},
		{"spf invalid ip4", "spf", "fail", "", spf("v=spf1 ip4:300.1.1.1 -all")},
		{"spf timeout", "spf", "unknown", "", func(f *fakeDNS) { f.errs[domain+"."] = &net.DNSError{IsTimeout: true} }},
		{"spf host lookup fails", "spf", "unknown", "", func(f *fakeDNS) { f.errs["mail.example.test."] = servfail }},
		{"dmarc quarantine", "dmarc", "pass", "", nil},
		{"dmarc reject", "dmarc", "pass", "", dmarcTXT("v=DMARC1; p=reject")},
		{"dmarc none", "dmarc", "fail", "", func(f *fakeDNS) { delete(f.txt, "_dmarc."+domain+".") }},
		{"dmarc several", "dmarc", "fail", "", dmarcTXT("v=DMARC1; p=reject", "v=DMARC1; p=none")},
		{"dmarc p=none", "dmarc", "warn", "", dmarcTXT("v=DMARC1; p=none")},
		{"dmarc pct", "dmarc", "warn", "", dmarcTXT("v=DMARC1; p=reject; pct=50")},
		{"dmarc unparseable", "dmarc", "fail", "", dmarcTXT("v=DMARC1; p=bogus")},
		{"dmarc servfail", "dmarc", "unknown", "", func(f *fakeDNS) { f.errs["_dmarc."+domain+"."] = servfail }},
		{"dmarc organizational", "dmarc", "pass", "news.example.com", func(f *fakeDNS) {
			delete(f.txt, "_dmarc.news.example.com.")
			f.txt["_dmarc.example.com."] = []string{"v=DMARC1; p=quarantine; sp=reject"}
		}},
		{"dmarc organizational sp=none", "dmarc", "warn", "news.example.com", func(f *fakeDNS) {
			delete(f.txt, "_dmarc.news.example.com.")
			f.txt["_dmarc.example.com."] = []string{"v=DMARC1; p=reject; sp=none"}
		}},
		{"ptr pass", "ptr", "pass", "", nil},
		{"ptr some", "ptr", "warn", "", func(f *fakeDNS) { f.ips = append(f.ips, net.ParseIP("203.0.113.6")) }},
		{"ptr wrong name", "ptr", "fail", "", func(f *fakeDNS) { f.ptr["203.0.113.5"] = []string{"host.provider.example."} }},
		{"ptr no address", "ptr", "fail", "", func(f *fakeDNS) { f.ips = nil }},
		{"spf no address", "spf", "fail", "", func(f *fakeDNS) { f.ips = nil }},
		{"ptr servfail", "ptr", "unknown", "", func(f *fakeDNS) { f.errs["203.0.113.5"] = servfail }},
		{"ptr host timeout", "ptr", "unknown", "", func(f *fakeDNS) { f.errs["mail.example.test."] = &net.DNSError{IsTimeout: true} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.domain
			if d == "" {
				d = domain
			}
			f := passingDNS(t, d)
			if c.edit != nil {
				c.edit(f)
			}
			result, err := deliverabilityProfile(d).CheckDeliverability(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			got := checkOf(t, result, c.check)
			if got.Status != c.want || got.Message == "" || result.EgressObserved || result.MailSent {
				t.Fatalf("%s: got %s (%s), want %s", c.check, got.Status, got.Message, c.want)
			}
			dnsFailure := strings.Contains(c.name, "servfail") || strings.Contains(c.name, "timeout") || strings.Contains(c.name, "fails")
			if dnsFailure && !strings.Contains(got.Message, "run the check again") {
				t.Fatal("unknown without a retry message", got.Message)
			}
			if c.name == "dmarc organizational" && got.Name != "_dmarc.example.com" {
				t.Fatal("organizational fallback not reported", got.Name)
			}
		})
	}
}

func TestDeliverabilityBounds(t *testing.T) {
	t.Run("lookup deadlines", func(t *testing.T) {
		f := passingDNS(t, "example.test")
		if _, err := deliverabilityProfile("example.test").CheckDeliverability(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		for _, d := range f.deadlines {
			if d <= 0 || d > 5*time.Second {
				t.Fatal("lookup without a 5-second timeout", d)
			}
		}
	})
	t.Run("slow resolver", func(t *testing.T) {
		lookup, budget := dnsLookupTimeout, deliverabilityBudget
		t.Cleanup(func() { dnsLookupTimeout, deliverabilityBudget = lookup, budget })
		dnsLookupTimeout, deliverabilityBudget = 40*time.Millisecond, 100*time.Millisecond
		f := passingDNS(t, "example.test")
		f.block = true
		start := time.Now()
		result, err := deliverabilityProfile("example.test").CheckDeliverability(context.Background(), f)
		if err != nil || time.Since(start) > 2*time.Second {
			t.Fatal("request budget ignored", time.Since(start), err)
		}
		for _, c := range append(result.Domains[0].Checks, result.PTR) {
			if c.Status != "unknown" {
				t.Fatal("slow DNS reported", c.Check, c.Status)
			}
		}
	})
	t.Run("at most 16 reverse lookups", func(t *testing.T) {
		f := passingDNS(t, "example.test")
		f.ips = nil
		var cover []string
		for i := 1; i <= 20; i++ {
			ip := net.IPv4(203, 0, 113, byte(i))
			f.ips = append(f.ips, ip)
			f.ptr[ip.String()] = []string{"mail.example.test."}
			cover = append(cover, "ip4:"+ip.String())
		}
		f.txt["example.test."] = []string{"v=spf1 " + strings.Join(cover, " ") + " -all"}
		result, err := deliverabilityProfile("example.test").CheckDeliverability(context.Background(), f)
		if err != nil || f.addrCalls != 16 || result.PTR.Status != "warn" || checkOf(t, result, "spf").Status != "pass" {
			t.Fatal("reverse lookups unbounded", f.addrCalls, result.PTR, err)
		}
	})
	t.Run("echoed text truncated", func(t *testing.T) {
		f := passingDNS(t, "example.test")
		k := testDKIMKey("example.test")
		long := "v=DKIM1; n=" + strings.Repeat("\xffx", 1500) + "; p=" + strings.Repeat("A", 100)
		f.txt[k.recordName()+"."] = []string{long}
		f.txt["example.test."] = []string{"v=spf1 " + strings.Repeat("ip4:198.51.100.1 ", 100) + "-all"}
		f.txt["_dmarc.example.test."] = []string{"v=DMARC1; p=reject; rua=mailto:" + strings.Repeat("a", 2000) + "@example.test"}
		result, err := deliverabilityProfile("example.test").CheckDeliverability(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range result.Domains[0].Checks {
			if c.Found == "" || len(c.Found) > 512 || len(c.Message) > 1024 {
				t.Fatal("DNS text not bounded", c.Check, len(c.Found), len(c.Message))
			}
		}
	})
	t.Run("relay mode refused", func(t *testing.T) {
		f := passingDNS(t, "example.test")
		c := DomainRelay{Generation: directTestProfile().Generation, Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"}
		if _, err := c.CheckDeliverability(context.Background(), f); err == nil || f.calls != 0 {
			t.Fatal("relay profile checked", err, f.calls)
		}
	})
}
