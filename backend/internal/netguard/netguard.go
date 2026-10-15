// Package netguard holds the single definition of "this IP is somewhere inside
// the deployment and must never be reached by an outbound request made on a
// user's behalf".
//
// It is a leaf package with no dependencies on the rest of this codebase
// because two packages need it and neither can import the other: internal/api
// guards user-supplied CardDAV server URLs, and internal/processor guards
// client-supplied UnifiedPush endpoint URLs. Those two had a copy each,
// character-identical and separately maintained, and they had already drifted
// out of correctness together — neither classified RFC 6598 shared address
// space, so on a box joined to a tailnet (where every node gets a 100.64/10
// address) both surfaces could reach the operator's whole private network.
// A security predicate with two homes is a predicate that will be fixed in one
// of them.
package netguard

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
)

// extraReservedCIDRs are ranges Go's own net.IP predicates do not classify but
// which are just as much "inside" as RFC1918 is.
//
// 100.64.0.0/10 is the one that matters: RFC 6598 shared address space, and
// what Tailscale assigns to every node on a tailnet — a very common way to run
// a self-hosted server like this one. net.IP.IsPrivate returns false for it, so
// it has to be listed explicitly.
//
// The rest are small and cost nothing to refuse: IETF protocol assignments,
// benchmarking space, and the TEST-NET ranges, none of which is ever a real
// destination. The two IPv6 entries matter for a subtler reason — both embed an
// arbitrary IPv4 address (including a private one) inside an address that looks
// global, so a NAT64 or 6to4 literal is a way to smuggle 10.0.0.1 past a check
// that only inspects IPv6 predicates.
var extraReservedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",       // RFC 1122 "this network"; Linux routes 0.x to the host
		"100.64.0.0/10",   // RFC 6598 CGNAT / Tailscale
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved, and 255.255.255.255 broadcast
		"64:ff9b::/96",    // RFC 6052 NAT64 — wraps arbitrary IPv4
		"64:ff9b:1::/48",  // RFC 8215 local-use NAT64 — likewise
		"2002::/16",       // 6to4 — likewise
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsPrivateOrReserved reports whether ip must never be reached via a
// user-supplied outbound URL: loopback, RFC1918/RFC4193 private, link-local
// (which also covers the 169.254.169.254 cloud metadata address), multicast,
// unspecified, or any of extraReservedCIDRs.
func IsPrivateOrReserved(ip net.IP) bool {
	if ip == nil {
		// Not a resolvable destination. Refusing is the safe answer for a
		// value a caller failed to parse.
		return true
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	// Match in 4-byte form when there is one, so an IPv4-mapped IPv6 literal
	// (::ffff:100.64.0.1) is caught by the IPv4 CIDRs rather than sliding past
	// them, and in 16-byte form as well for the two IPv6 entries.
	if v4 := ip.To4(); v4 != nil {
		for _, n := range extraReservedCIDRs {
			if n.Contains(v4) {
				return true
			}
		}
	}
	for _, n := range extraReservedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowedHosts are hostnames an operator lets resolve to internal addresses
// (SANDBOX_PRIVATE_HOSTS): a LAN mail server, a sandbox CardDAV host.
var AllowedHosts = ParseHosts(os.Getenv("SANDBOX_PRIVATE_HOSTS"))

// ParseHosts reads a comma-separated host list. IP literals are dropped: one
// entry must never make a private address generally reachable.
func ParseHosts(raw string) map[string]struct{} {
	hosts := make(map[string]struct{})
	for _, h := range strings.Split(raw, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || net.ParseIP(h) != nil {
			continue
		}
		hosts[h] = struct{}{}
	}
	return hosts
}

// HostAllowed reports whether host is listed: exact match after case folding,
// so a name an attacker controls cannot swallow an entry by extending it.
func HostAllowed(allowed map[string]struct{}, host string) bool {
	if len(allowed) == 0 || host == "" || net.ParseIP(host) != nil {
		return false
	}
	_, ok := allowed[strings.ToLower(host)]
	return ok
}

// ErrInternalAddress is returned by Dialer for a refused destination.
var ErrInternalAddress = errors.New("destination resolves to a private or reserved address")

// DialContext is Dialer(AllowedHosts).
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return Dialer(AllowedHosts)(ctx, network, addr)
}

// Dialer resolves the host once, refuses it if ANY address is internal, then
// dials the checked addresses themselves, so DNS cannot answer differently in
// between. Allowed hosts are dialled by name.
func Dialer(allowed map[string]struct{}) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		if HostAllowed(allowed, host) {
			return d.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if IsPrivateOrReserved(ip) {
				return nil, ErrInternalAddress
			}
		}
		lastErr := ErrInternalAddress // no addresses at all
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
