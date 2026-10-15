package config

import (
	"errors"
	"net"
	"os"
	"strings"
)

// TrustedProxies is which peers may name the client address, and the one
// header they name it in.
type TrustedProxies struct {
	Nets []*net.IPNet
	// Header is the canonical header name: X-Forwarded-For or CF-Connecting-IP.
	Header string
}

// LoadTrustedProxies reads TRUSTED_PROXY_CIDRS and TRUSTED_PROXY_HEADER.
func LoadTrustedProxies() (TrustedProxies, error) {
	return ParseTrustedProxies(os.Getenv("TRUSTED_PROXY_CIDRS"), os.Getenv("TRUSTED_PROXY_HEADER"))
}

// ParseTrustedProxies parses comma-separated CIDRs or bare IPs, and a header of
// x-forwarded-for (default) or cf-connecting-ip. Any malformed value is an
// error: a typo must not change who is trusted.
func ParseTrustedProxies(cidrs, header string) (TrustedProxies, error) {
	var p TrustedProxies
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "", "x-forwarded-for":
		p.Header = "X-Forwarded-For"
	case "cf-connecting-ip":
		p.Header = "CF-Connecting-IP"
	default:
		return p, errors.New("TRUSTED_PROXY_HEADER must be x-forwarded-for or cf-connecting-ip")
	}
	for _, field := range strings.Split(cidrs, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(field); err == nil {
			p.Nets = append(p.Nets, n)
			continue
		}
		ip := net.ParseIP(field)
		if ip == nil {
			return TrustedProxies{}, errors.New("TRUSTED_PROXY_CIDRS: " + field + " is not a CIDR or IP address")
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		p.Nets = append(p.Nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return p, nil
}
