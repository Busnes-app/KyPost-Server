// Package suitepair is one KyPost ↔ suite-app pairing (docs/KYCALENDAR.md
// "Pairing"): the environment's URL and secret file, or KyIdentity's suite
// directory, and the signed client for KyPost → peer requests.
//
// ponytail: internal/kycalendar and internal/kydrive still carry their own
// copies of this pairing; fold them into this package in a change of their own.
package suitepair

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// MinSecretBytes is the shortest shared secret accepted, after trimming.
const MinSecretBytes = 32

// maxResponseBytes caps a peer's answer; they are a few bytes of JSON.
const maxResponseBytes = 64 << 10

// ErrOff is a request made while the peer is not paired.
var ErrOff = errors.New("suitepair: not paired")

// Peer names the app KyPost pairs with.
type Peer struct {
	ID   string // KyIdentity client ID, e.g. "kymessage"
	Name string // shown to administrators, e.g. "KyMessages"
	Env  string // variable prefix, e.g. "KYPOST_KYMESSAGE"
}

// Client signs requests to the peer. It is paired by the environment for the
// life of the process, or through KyIdentity by Run.
type Client struct {
	peer Peer
	link atomic.Pointer[link]
	http *http.Client
	dir  *directory // nil when the environment paired it
}

type link struct {
	base   *url.URL
	secret []byte
	source string
}

// FromEnv reads <Env>_URL, <Env>_SECRET_FILE and <Env>_ALLOW_PRIVATE. Both
// unset returns a Client that Run pairs through KyIdentity; anything else
// unusable is an error, so the process refuses to start half paired.
func FromEnv(p Peer) (*Client, error) {
	rawURL := strings.TrimSpace(os.Getenv(p.Env + "_URL"))
	file := strings.TrimSpace(os.Getenv(p.Env + "_SECRET_FILE"))
	allowPrivate, err := config.EnvBool(p.Env + "_ALLOW_PRIVATE")
	if err != nil {
		return nil, err
	}
	if rawURL == "" && file == "" {
		return newDirectoryClient(p, allowPrivate), nil
	}
	if rawURL == "" || file == "" {
		return nil, fmt.Errorf("%s_URL and %s_SECRET_FILE must be set together", p.Env, p.Env)
	}
	base, err := p.parseBase(rawURL)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s_SECRET_FILE cannot be read", p.Env)
	}
	secret := bytes.TrimSpace(raw)
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("%s_SECRET_FILE must hold at least %d bytes", p.Env, MinSecretBytes)
	}
	return New(p, base, secret, guardedTransport(allowPrivate)), nil
}

// parseBase accepts only https://host[:port], never a loopback literal.
func (p Peer) parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%s_URL must be https://host with no path", p.Env)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() || strings.EqualFold(u.Hostname(), "localhost") {
		return nil, fmt.Errorf("%s_URL must not be a loopback address", p.Env)
	}
	u.Path = ""
	return u, nil
}

// New builds a Client paired with base and secret over rt. Tests pass an
// httptest server's transport.
func New(p Peer, base *url.URL, secret []byte, rt http.RoundTripper) *Client {
	c := newClient(p, rt)
	c.link.Store(&link{base: base, secret: secret, source: SourceEnv})
	return c
}

func newClient(p Peer, rt http.RoundTripper) *Client {
	return &Client{
		peer: p,
		http: &http.Client{
			Transport: &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes},
			Timeout:   30 * time.Second,
			// A redirect would replay the signed body somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// guardedTransport dials the peer only at checked addresses: never loopback,
// unspecified, link-local or multicast, and other private or reserved ranges
// only with allowPrivate. The host is resolved once and the checked address
// dialled.
func guardedTransport(allowPrivate bool) http.RoundTripper {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if refused(ip, allowPrivate) {
				return nil, netguard.ErrInternalAddress
			}
		}
		var d net.Dialer
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		lastErr := netguard.ErrInternalAddress
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
}

func refused(ip net.IP, allowPrivate bool) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	return !allowPrivate && netguard.IsPrivateOrReserved(ip)
}

// Available reports whether the peer is paired right now. Nil-safe.
func (c *Client) Available() bool { return c != nil && c.link.Load() != nil }

// Post signs body as eventType/eventID and POSTs it to path on the peer,
// returning the status and the response body. Nil-safe. A bad_signature answer asks
// Run to re-resolve the pairing: the peer may hold a rotated secret.
func (c *Client) Post(ctx context.Context, path, eventType, eventID string, body any) (int, []byte, error) {
	if c == nil {
		return 0, nil, ErrOff
	}
	l := c.link.Load()
	if l == nil {
		return 0, nil, ErrOff
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	h, err := syncauth.Sign(l.secret, time.Now(), eventType, eventID, payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base.JoinPath(path).String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	h.Apply(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, nil, fmt.Errorf("%s: %w", c.peer.ID, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: read answer: %w", c.peer.ID, err)
	}
	if resp.StatusCode == http.StatusUnauthorized && Outcome(out) == "bad_signature" {
		c.Refresh()
	}
	return resp.StatusCode, out, nil
}

// Outcome reads the "outcome" or "code" of an answer; anything but a short
// [a-z0-9_] token reads as "".
func Outcome(body []byte) string {
	var v struct {
		Outcome string `json:"outcome"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(body, &v)
	out := cmp.Or(v.Outcome, v.Code)
	if len(out) > 64 || strings.ContainsFunc(out, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' }) {
		return ""
	}
	return out
}
