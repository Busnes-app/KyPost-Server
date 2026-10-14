package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsPrivateOrReservedRefusesInternalDestinations(t *testing.T) {
	for _, tc := range []struct{ ip, why string }{
		{"127.0.0.1", "loopback"},
		{"::1", "IPv6 loopback"},
		{"10.1.2.3", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"fd00::1", "RFC4193 unique local"},
		{"169.254.169.254", "cloud metadata"},
		{"0.0.0.0", "unspecified"},
		{"224.0.0.1", "multicast"},

		// The gap this package was extracted to close: both call sites had
		// their own copy of the predicate and neither classified CGNAT, so a
		// tailnet was reachable from a user-supplied URL.
		{"100.64.0.1", "RFC 6598 CGNAT / Tailscale"},
		{"100.100.100.100", "Tailscale MagicDNS"},
		{"100.127.255.254", "top of CGNAT range"},
		{"::ffff:100.64.0.1", "CGNAT as an IPv4-mapped IPv6 literal"},

		{"192.0.0.1", "IETF protocol assignments"},
		{"198.18.0.1", "benchmarking"},
		{"192.0.2.1", "TEST-NET-1"},
		{"198.51.100.1", "TEST-NET-2"},
		{"203.0.113.1", "TEST-NET-3"},
		{"64:ff9b::a00:1", "NAT64-wrapped 10.0.0.1"},
		{"2002:a00:1::", "6to4-wrapped 10.0.0.1"},
	} {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("test bug: %q is not a parseable IP", tc.ip)
		}
		if !IsPrivateOrReserved(ip) {
			t.Errorf("IsPrivateOrReserved(%s) = false, want true (%s)", tc.ip, tc.why)
		}
	}
}

// The guard must stay narrow enough that ordinary public destinations — a real
// CardDAV host, a real UnifiedPush server — still work.
func TestIsPrivateOrReservedAllowsPublicDestinations(t *testing.T) {
	for _, addr := range []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",
		"99.255.255.255",  // immediately below 100.64/10
		"100.63.255.255",  // ditto, boundary
		"100.128.0.0",     // immediately above 100.64/10
		"2606:4700::1111", // public IPv6
	} {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("test bug: %q is not a parseable IP", addr)
		}
		if IsPrivateOrReserved(ip) {
			t.Errorf("IsPrivateOrReserved(%s) = true, want false — this is a public address", addr)
		}
	}
}

// A nil IP means the caller could not parse what it was given; refusing is the
// only safe answer, and it must not panic.
func TestIsPrivateOrReservedRefusesNil(t *testing.T) {
	if !IsPrivateOrReserved(nil) {
		t.Fatal("IsPrivateOrReserved(nil) = false, want true")
	}
}

// The dial guard refuses internal destinations by literal and by name, and an
// operator-listed hostname is the only way past it.
func TestDialerRefusesInternalUnlessAllowed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	refuse := Dialer(ParseHosts("127.0.0.1, 169.254.169.254")) // literals never count
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "::1", "localhost"} {
		conn, err := refuse(ctx, "tcp", net.JoinHostPort(host, port))
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, ErrInternalAddress) {
			t.Errorf("dial %s: %v, want ErrInternalAddress", host, err)
		}
	}

	conn, err := Dialer(ParseHosts("LocalHost"))(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("allowlisted localhost: %v", err)
	}
	conn.Close()

	// A lookalike of an allowed name is checked like any other name.
	stubLookup(t, []net.IP{net.ParseIP("127.0.0.1")})
	if _, err := Dialer(ParseHosts("localhost"))(ctx, "tcp", net.JoinHostPort("localhost.evil.example", port)); !errors.Is(err, ErrInternalAddress) {
		t.Fatalf("lookalike of an allowed host: %v, want ErrInternalAddress", err)
	}
}

// stubLookup answers every lookup with the next answer, counting calls.
func stubLookup(t *testing.T, answers ...[]net.IP) *int {
	t.Helper()
	calls := 0
	previous := lookupIP
	lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		calls++
		return answers[min(calls, len(answers))-1], nil
	}
	t.Cleanup(func() { lookupIP = previous })
	return &calls
}

// One internal address among public ones refuses the host: a rebinding answer
// cannot hide a private address behind a public one.
func TestDialerRefusesMixedAnswer(t *testing.T) {
	stubLookup(t, []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")})
	if _, err := Dialer(nil)(context.Background(), "tcp", "mixed.example:443"); !errors.Is(err, ErrInternalAddress) {
		t.Fatalf("mixed answer: %v, want ErrInternalAddress", err)
	}
}

// DNS rebinding: the dial uses the address that was checked, never a second
// resolution of the name. A cancelled context stops the dial before any packet
// is sent; the error still names the address it was given.
func TestDialerDialsTheCheckedAddress(t *testing.T) {
	calls := stubLookup(t, []net.IP{net.ParseIP("8.8.8.8")}, []net.IP{net.ParseIP("127.0.0.1")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Dialer(nil)(ctx, "tcp", "rebind.example:443")
	var op *net.OpError
	if !errors.As(err, &op) || op.Addr == nil || op.Addr.String() != "8.8.8.8:443" {
		t.Fatalf("dial error %v, want one naming 8.8.8.8:443", err)
	}
	if *calls != 1 {
		t.Fatalf("resolved %d times, want once", *calls)
	}
}

func TestBoundedTransportCapsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("A", 4096))
	}))
	defer srv.Close()
	client := &http.Client{Transport: &BoundedTransport{Base: http.DefaultTransport, Limit: 100}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) != 100 {
		t.Fatalf("read %d bytes (%v), want 100", len(body), err)
	}
}
