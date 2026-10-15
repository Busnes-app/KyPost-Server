package config

import (
	"net"
	"testing"
)

func TestParseTrustedProxiesHeader(t *testing.T) {
	for raw, want := range map[string]string{
		"":                   "X-Forwarded-For",
		"x-forwarded-for":    "X-Forwarded-For",
		" CF-Connecting-IP ": "CF-Connecting-IP",
		"cf-connecting-ip":   "CF-Connecting-IP",
	} {
		p, err := ParseTrustedProxies("", raw)
		if err != nil || p.Header != want {
			t.Errorf("header %q: got %q, %v; want %q", raw, p.Header, err, want)
		}
	}
	for _, bad := range []string{"x-real-ip", "forwarded", "both", "x-forwarded-for,cf-connecting-ip"} {
		if _, err := ParseTrustedProxies("", bad); err == nil {
			t.Errorf("header %q accepted; an unknown value must fail startup", bad)
		}
	}
}

func TestParseTrustedProxiesCIDRs(t *testing.T) {
	p, err := ParseTrustedProxies(" 10.89.0.0/24, 172.18.0.1 ,::1/128,", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"10.89.0.7", "172.18.0.1", "::1"} {
		if !contains(p.Nets, ip) {
			t.Errorf("%s not trusted", ip)
		}
	}
	if contains(p.Nets, "172.18.0.2") {
		t.Error("bare IP widened to more than one host")
	}

	for _, bad := range []string{"not-an-ip", "10.0.0.0/8, 10.89.0.1/33", "proxy.example"} {
		if _, err := ParseTrustedProxies(bad, ""); err == nil {
			t.Errorf("TRUSTED_PROXY_CIDRS=%q accepted; a malformed entry must fail startup", bad)
		}
	}
}

func TestLoadTrustedProxiesReadsEnv(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1")
	t.Setenv("TRUSTED_PROXY_HEADER", "cf-connecting-ip")
	p, err := LoadTrustedProxies()
	if err != nil || p.Header != "CF-Connecting-IP" || !contains(p.Nets, "127.0.0.1") {
		t.Fatalf("got %+v, %v", p, err)
	}
	t.Setenv("TRUSTED_PROXY_HEADER", "x-real-ip")
	if _, err := LoadTrustedProxies(); err == nil {
		t.Fatal("unknown TRUSTED_PROXY_HEADER accepted")
	}
}

func contains(nets []*net.IPNet, ip string) bool {
	for _, n := range nets {
		if n.Contains(net.ParseIP(ip)) {
			return true
		}
	}
	return false
}
