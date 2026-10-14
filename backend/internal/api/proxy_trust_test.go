package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// trustProxiesForTest applies proxy trust for the duration of a test, as
// SetTrustedProxies does at startup.
func trustProxiesForTest(t *testing.T, cidrs, header string) {
	t.Helper()
	p, err := config.ParseTrustedProxies(cidrs, header)
	if err != nil {
		t.Fatal(err)
	}
	oldNets, oldHeader := trustedProxyNets, clientIPHeader
	SetTrustedProxies(p)
	t.Cleanup(func() { trustedProxyNets, clientIPHeader = oldNets, oldHeader })
}

// trustProxyCIDRsForTest trusts cidrs in the default X-Forwarded-For mode.
func trustProxyCIDRsForTest(t *testing.T, cidrs string) {
	t.Helper()
	trustProxiesForTest(t, cidrs, "")
}

func forwardedRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/health", nil)
	req.RemoteAddr = "203.0.113.50:40000"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-For", "10.0.0.99")
	return req
}

// Default behavior (no trusted proxies configured): the listener may be reached
// directly, so X-Forwarded-* must NOT be trusted — otherwise any client can
// forge its own IP and defeat the login/CardDAV lockouts. This pins the
// fail-closed default.
func TestProxyHeadersIgnoredByDefault(t *testing.T) {
	trustProxyCIDRsForTest(t, "")
	req := forwardedRequest(t)
	if isRequestSecure(req) {
		t.Fatal("default: a forged X-Forwarded-Proto must not mark a plain-HTTP request secure")
	}
	if got := clientIP(req); got != "203.0.113.50" {
		t.Fatalf("default: clientIP = %q, want the connection's own address", got)
	}
}

// A request arriving FROM a configured trusted proxy has its forwarded headers
// honored.
func TestProxyHeadersTrustedFromConfiguredProxy(t *testing.T) {
	// 203.0.113.50 is the peer in forwardedRequest.
	trustProxyCIDRsForTest(t, "203.0.113.0/24")
	req := forwardedRequest(t)
	if !isRequestSecure(req) {
		t.Fatal("trusted peer: X-Forwarded-Proto=https should mark the request secure")
	}
	if got := clientIP(req); got != "10.0.0.99" {
		t.Fatalf("trusted peer: clientIP = %q, want the forwarded address", got)
	}
}

// THE REGRESSION TEST FOR THE REAL BUG: forwarded headers must be ignored when
// the request did not come from a trusted proxy, even though trust is
// configured.
//
// The old TRUST_PROXY_HEADERS=true was a global boolean, so it trusted headers
// on every connection from any peer. The shipped compose file published 5866 on
// 0.0.0.0 and the README told operators to set the flag, so an attacker
// connecting straight to 5866 — bypassing the proxy entirely — got to name its
// own IP and thereby defeat every IP-keyed lockout and rate limit at once.
func TestProxyHeadersIgnoredFromUntrustedPeer(t *testing.T) {
	// A proxy is configured, but it is not the peer making this request.
	trustProxyCIDRsForTest(t, "10.10.0.0/16")
	req := forwardedRequest(t) // peer 203.0.113.50, outside the trusted range

	if isRequestSecure(req) {
		t.Error("untrusted peer: a forged X-Forwarded-Proto must not mark the request secure")
	}
	if got := clientIP(req); got != "203.0.113.50" {
		t.Errorf("untrusted peer: clientIP = %q, want the peer address — a caller who can choose "+
			"this value has defeated every lockout keyed on it", got)
	}
}

// Every X-Forwarded-* header must be read at the RIGHT-most hop, not just
// X-Forwarded-For.
//
// clientIP already read the right-most element because the left-most is
// client-prepended; isRequestSecure, in the same file, read the left-most. That
// asymmetry meant the Secure flag on the session cookie and the HSTS header
// were decided by a value the client supplies.
func TestForwardedHeadersAllUseRightmostHop(t *testing.T) {
	trustProxyCIDRsForTest(t, "203.0.113.0/24")
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
	req.RemoteAddr = "203.0.113.9:5000"

	// In each case the left-most value is what a client prepended and the
	// right-most is what the trusted proxy actually appended.
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.77")
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "attacker.example, real.example")

	if got := clientIP(req); got != "203.0.113.77" {
		t.Errorf("clientIP = %q, want the right-most hop 203.0.113.77 (not the client-prepended 1.1.1.1)", got)
	}
	if isRequestSecure(req) {
		t.Error("isRequestSecure honored the client-prepended https over the proxy's own http; " +
			"this decides the session cookie's Secure flag")
	}
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		peer  string
		trust bool
	}{
		{"cidr match", "10.0.0.0/8", "10.1.2.3", true},
		{"cidr miss", "10.0.0.0/8", "192.168.1.1", false},
		// A bare IP is what a compose sidecar proxy looks like.
		{"bare ip match", "172.18.0.1", "172.18.0.1", true},
		{"bare ip miss", "172.18.0.1", "172.18.0.2", false},
		{"ipv6 cidr", "::1/128", "::1", true},
		{"multiple entries", "10.0.0.0/8, 172.18.0.1", "172.18.0.1", true},
		{"whitespace tolerated", "  10.0.0.0/8  ", "10.9.9.9", true},
		{"empty trusts nothing", "", "10.0.0.1", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trustProxyCIDRsForTest(t, c.raw)
			req := httptest.NewRequest(http.MethodGet, "http://x/api/y", nil)
			req.RemoteAddr = net.JoinHostPort(c.peer, "1234")
			req.Header.Set("X-Forwarded-For", "9.9.9.9")

			got := clientIP(req) == "9.9.9.9"
			if got != c.trust {
				t.Errorf("TRUSTED_PROXY_CIDRS=%q peer=%s: headers trusted = %v, want %v",
					c.raw, c.peer, got, c.trust)
			}
		})
	}
}

// Behind nginx (XFF mode) the client can send its own CF-Connecting-IP, which
// nginx passes through. Honouring it let one caller pick a fresh lockout key per
// request.
func TestSpoofedCFConnectingIPIgnoredInForwardedForMode(t *testing.T) {
	trustProxyCIDRsForTest(t, "10.89.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
	req.RemoteAddr = "10.89.0.1:41000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want the X-Forwarded-For address 203.0.113.9", got)
	}

	req.Header.Del("X-Forwarded-For")
	if got := clientIP(req); got != "10.89.0.1" {
		t.Errorf("clientIP = %q without X-Forwarded-For, want the peer, not CF-Connecting-IP", got)
	}
}

// cloudflared appends its own hop to X-Forwarded-For, so in cf-connecting-ip mode
// only CF-Connecting-IP names the client, and XFF is never read.
func TestCFConnectingIPModeReadsOnlyCFConnectingIP(t *testing.T) {
	trustProxiesForTest(t, "127.0.0.1/32", "cf-connecting-ip")
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
	req.RemoteAddr = "127.0.0.1:41000"
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 127.0.0.1")
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want CF-Connecting-IP 203.0.113.9", got)
	}

	req.Header.Del("CF-Connecting-IP")
	if got := clientIP(req); got != "127.0.0.1" {
		t.Errorf("clientIP = %q without CF-Connecting-IP, want the peer, not X-Forwarded-For", got)
	}
}

func TestCFConnectingIPIgnoredFromUntrustedPeer(t *testing.T) {
	// A configured proxy that is not this peer.
	trustProxiesForTest(t, "10.0.0.0/8", "cf-connecting-ip")
	req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
	req.RemoteAddr = "203.0.113.50:40000"
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")

	if got := clientIP(req); got != "203.0.113.50" {
		t.Errorf("clientIP = %q, want the peer address: CF-Connecting-IP is just a header and any "+
			"client can send one", got)
	}
}

// TestClientIPFallsBackWhenHeadersDoNotParse guards the lockout-key space.
//
// The resolved value becomes a key in a bounded map. A trusted-but-buggy proxy
// sending "unknown", an empty element from a trailing comma, or arbitrary text
// would otherwise mint keys that match nothing real and grow without limit.
func TestClientIPFallsBackWhenHeadersDoNotParse(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{"unknown literal", "CF-Connecting-IP", "unknown", "127.0.0.1"},
		{"empty trailing element", "X-Forwarded-For", "203.0.113.1, ", "127.0.0.1"},
		{"arbitrary text", "X-Forwarded-For", "also-not-an-ip", "127.0.0.1"},
		// Proxies in the wild send ip:port; accept it rather than
		// discarding a perfectly good address.
		{"xff with port", "X-Forwarded-For", "203.0.113.8:5555", "203.0.113.8"},
		{"cf with port", "CF-Connecting-IP", "203.0.113.9:443", "203.0.113.9"},
		{"ipv6 bare", "CF-Connecting-IP", "2001:db8::1", "2001:db8::1"},
		{"ipv6 bracketed with port", "CF-Connecting-IP", "[2001:db8::2]:443", "2001:db8::2"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trustProxiesForTest(t, "127.0.0.1/32", c.header)
			req := httptest.NewRequest(http.MethodGet, "http://backend.internal/api/x", nil)
			req.RemoteAddr = "127.0.0.1:41000"
			req.Header.Set(c.header, c.value)
			if got := clientIP(req); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// TestStatusReportsResolvedClientIP covers the diagnostic.
//
// It exists because there was no way to verify a proxy deployment: nothing logs
// the client IP (see log_privacy_test.go), so an operator had no way to tell
// whether the lockouts were keying off real callers or off the proxy — a failure
// that is silent in both directions.
func TestStatusReportsResolvedClientIP(t *testing.T) {
	srv := newTestServer(t)
	u, err := users0(t, srv)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	call := func() (string, bool) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.RemoteAddr = "127.0.0.1:41000"
		req.Header.Set("CF-Connecting-IP", "203.0.113.42")
		rec := httptest.NewRecorder()
		srv.handleStatus(rec, authedRequestForTest(req, u))
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			ClientIP            string `json:"clientIp"`
			ProxyHeadersTrusted bool   `json:"proxyHeadersTrusted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return out.ClientIP, out.ProxyHeadersTrusted
	}

	// Untrusted peer: reports the connection address and says trust is off, which
	// is the signal that TRUSTED_PROXY_CIDRS needs setting.
	trustProxyCIDRsForTest(t, "")
	ip, trusted := call()
	if trusted {
		t.Error("proxyHeadersTrusted is true with no CIDRs configured")
	}
	if ip != "127.0.0.1" {
		t.Errorf("clientIp = %q, want the peer address 127.0.0.1", ip)
	}

	// Trusted peer: reports the real visitor address.
	trustProxiesForTest(t, "127.0.0.1/32", "cf-connecting-ip")
	ip, trusted = call()
	if !trusted {
		t.Error("proxyHeadersTrusted is false for a configured peer")
	}
	if ip != "203.0.113.42" {
		t.Errorf("clientIp = %q, want the visitor address 203.0.113.42", ip)
	}
}

// users0 creates a throwaway non-admin account for the status call.
func users0(t *testing.T, srv *Server) (users.User, error) {
	t.Helper()
	return srv.users.Create(context.Background(), "status-probe", "correct-horse-battery-staple", users.RoleUser)
}

// authedRequestForTest attaches an AuthContext, since handleStatus reads the
// caller from the context rather than re-authenticating.
func authedRequestForTest(req *http.Request, u users.User) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), authContextKey{},
		AuthContext{UserID: u.ID, Username: u.Username, Role: u.Role}))
}
