package mailmsg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// These seams are replaced only by sequential package tests. Runtime always
// resolves recipient MXs and dials the checked public IP, never the name again.
var directLookupMX = net.DefaultResolver.LookupMX
var directLookupIP = net.DefaultResolver.LookupIP
var directDial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
var directRoots *x509.CertPool

// Bound untrusted greeting/reply and TLS input independently of the clock.
// SMTP uploads are already bounded by the stored message's size limits.
type directSMTPConn struct {
	net.Conn
	reader io.Reader
}

func (c directSMTPConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func directRefusal(code int) error {
	return &textproto.Error{Code: code, Msg: "direct delivery unavailable; inspect owner-scoped outbox status before resubmitting"}
}

// directDeliver handles ONE recipient. The existing outbox owns every retry;
// no intermediary queue or automatic retry after DATA can erase uncertainty.
func (c DomainRelay) directDeliver(from string, recipients []string, raw []byte) error {
	if len(recipients) != 1 {
		return ErrDomainRelay
	}
	domain := strings.ToLower(recipients[0][strings.LastIndexByte(recipients[0], '@')+1:])
	if !relayDNSName(domain) || net.ParseIP(domain) != nil {
		return directRefusal(550)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	mxs, err := directLookupMX(ctx, domain+".")
	if err != nil {
		var dns *net.DNSError
		if !errors.As(err, &dns) || !dns.IsNotFound {
			return directRefusal(451)
		}
	}
	if len(mxs) == 0 {
		mxs = []*net.MX{{Host: domain + "."}}
	}
	if len(mxs) > 20 {
		return directRefusal(451)
	}
	sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	for _, mx := range mxs {
		if mx.Host == "." {
			if len(mxs) == 1 && mx.Pref == 0 {
				return directRefusal(550)
			}
			return directRefusal(451)
		}
	}
	for _, mx := range mxs {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		if !relayDNSName(host) || net.ParseIP(host) != nil {
			continue
		}
		ips, err := directLookupIP(ctx, "ip", host+".")
		if err != nil || len(ips) == 0 || len(ips) > 16 {
			continue
		}
		refused := false
		for _, ip := range ips {
			if netguard.IsPrivateOrReserved(ip) {
				refused = true
				break
			}
		}
		if refused {
			continue
		}
		for _, ip := range ips {
			client, err := c.directClient(ctx, host, ip)
			if err != nil {
				continue
			} // No envelope or DATA was sent: MX/address fallback is safe.
			defer client.Close()
			// Once the envelope starts, return its real result. In particular never
			// try another MX after an accepted or ambiguous final DATA response.
			return writeSMTPMessage(client, from, recipients, raw)
		}
	}
	return directRefusal(451)
}

func (c DomainRelay) directClient(ctx context.Context, host string, ip net.IP) (*smtp.Client, error) {
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := directDial(attempt, "tcp", net.JoinHostPort(ip.String(), "25"))
	if err != nil {
		return nil, err
	}
	conn = directSMTPConn{Conn: conn, reader: io.LimitReader(conn, 1<<20)}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	early := time.Now().Add(5 * time.Second)
	if deadline.Before(early) {
		early = deadline
	}
	if err = conn.SetDeadline(early); err != nil {
		return nil, err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, err
	}
	if err = client.Hello(c.Host); err != nil {
		return nil, err
	}
	if offered, _ := client.Extension("STARTTLS"); !offered {
		return nil, errors.New("direct delivery requires STARTTLS")
	}
	if err = client.StartTLS(&tls.Config{ServerName: host, RootCAs: directRoots, MinVersion: tls.VersionTLS12}); err != nil {
		return nil, err
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	ok = true
	return client, nil
}
