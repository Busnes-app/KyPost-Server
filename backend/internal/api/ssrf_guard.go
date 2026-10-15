package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// outboundIPGuard and outboundDial are variables solely so tests in this
// package can relax them to reach httptest's loopback listeners — production
// code must never reassign them.
var (
	outboundIPGuard = netguard.IsPrivateOrReserved
	outboundDial    = netguard.DialContext
)

// outboundCardDAVSchemes are the schemes a user-supplied CardDAV server URL
// may use.
//
// https only, and a var for the same single reason outboundIPGuard is one:
// httptest listeners speak plain http. Production must never widen it. Every
// request to a CardDAV URL carries the user's password in an HTTP Basic
// header, and outboundIPGuard already refuses loopback and private space, so
// an http:// target is necessarily a public host reached in the clear.
var outboundCardDAVSchemes = []string{"https"}

// validateOutboundURL rejects URLs that are not safe for this server to make
// requests to on a user's behalf: schemes outside allowedSchemes, and hosts
// that (as an IP literal or via DNS) resolve to a private/loopback/link-local
// address. Intended as an up-front check at configuration time; see
// netguard.Dialer for the dial-time recheck that also covers DNS
// rebinding and redirects.
func validateOutboundURL(rawURL string, allowedSchemes ...string) error {
	return validateOutboundURLWithSandbox(rawURL, netguard.AllowedHosts, allowedSchemes...)
}

func validateOutboundURLWithSandbox(rawURL string, allowed map[string]struct{}, allowedSchemes ...string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	schemeOK := false
	for _, s := range allowedSchemes {
		if strings.EqualFold(u.Scheme, s) {
			schemeOK = true
			break
		}
	}
	if !schemeOK {
		return fmt.Errorf("URL must use one of: %s", strings.Join(allowedSchemes, ", "))
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL missing host")
	}
	// After the scheme check, never before it: the allowlist relaxes where this
	// server may connect, not whether the user's password crosses the wire in
	// the clear. Resolution is skipped too, since a compose hostname does not
	// resolve outside the network it belongs to.
	if netguard.HostAllowed(allowed, host) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if outboundIPGuard(ip) {
			return errors.New("URL resolves to a private or reserved address")
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host: %w", err)
	}
	for _, ip := range ips {
		if outboundIPGuard(ip) {
			return fmt.Errorf("host resolves to a private or reserved address (%s)", ip)
		}
	}
	return nil
}

// maxOutboundResponseBytes bounds how much of a response from a user-supplied
// host this process will read.
//
// The CardDAV REPORT path applies its own 16 MiB LimitReader, but go-webdav's
// discovery PROPFINDs decode straight from resp.Body with none — upstream even
// carries a TODO noting the response can be quite large — and the walk-up loop
// repeats discovery per candidate path. XML decoding into a multistatus struct
// costs several times the wire size in resident memory, so an unbounded
// response from a host the user configured is a memory-exhaustion primitive
// against the shared container.
//
// Bounding at the transport rather than at each call site is what makes this
// cover every current and future go-webdav call at once.
const maxOutboundResponseBytes = 32 << 20

// newSSRFSafeHTTPClient builds an http.Client for outbound requests whose
// destination host is supplied by a user (e.g. a CardDAV server URL): every
// dial, including ones made for redirects, goes through netguard.DialContext,
// and every response body is bounded at maxOutboundResponseBytes.
func newSSRFSafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &netguard.BoundedTransport{
			// One transport per call: pooled conns would never be reused, only leaked.
			Base:  &http.Transport{DialContext: outboundDial, DisableKeepAlives: true},
			Limit: maxOutboundResponseBytes,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
				return fmt.Errorf("refusing cross-origin redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}
