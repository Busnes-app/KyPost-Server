// Package kycalendar is KyPost's half of the KyCalendar invitation-mail
// contract (docs/KYCALENDAR.md): the shared secret, the signed client for
// KyPost → KyCalendar requests, the receiver's replay guard, and the per-user
// record of invitations that arrived DKIM-aligned.
package kycalendar

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// Event types, one per route.
const (
	EventSend    = "kycalendar.itip.send"
	EventInbound = "kypost.itip.inbound"
	EventRespond = "kypost.itip.respond"
)

// MinSecretBytes is the shortest shared secret accepted, after trimming.
const MinSecretBytes = 32

// maxResponseBytes caps a KyCalendar answer; they are a few bytes of JSON.
const maxResponseBytes = 64 << 10

// ErrOff is a request made while KyCalendar is not paired.
var ErrOff = errors.New("kycalendar: not paired")

// Client signs requests to KyCalendar and verifies requests from it. It is
// paired by the environment for the life of the process, or through
// KyIdentity by Run; Available reports whether it is paired right now.
type Client struct {
	link   atomic.Pointer[link]
	http   *http.Client
	replay *guard
	dir    *directory // nil when the environment paired it
}

// link is one pairing: where KyCalendar is and the secret both sides sign with.
type link struct {
	base   *url.URL
	secret []byte
	source string
}

// FromEnv reads KYPOST_KYCALENDAR_URL, KYPOST_KYCALENDAR_SECRET_FILE and
// KYPOST_KYCALENDAR_ALLOW_PRIVATE. Both unset returns a Client that Run pairs
// through KyIdentity. Anything else that is not a usable configuration is an
// error, so the process refuses to start rather than run half paired.
func FromEnv() (*Client, error) {
	rawURL := strings.TrimSpace(os.Getenv("KYPOST_KYCALENDAR_URL"))
	file := strings.TrimSpace(os.Getenv("KYPOST_KYCALENDAR_SECRET_FILE"))
	allowPrivate, err := config.EnvBool("KYPOST_KYCALENDAR_ALLOW_PRIVATE")
	if err != nil {
		return nil, err
	}
	if rawURL == "" && file == "" {
		return newDirectoryClient(allowPrivate), nil
	}
	if rawURL == "" || file == "" {
		return nil, errors.New("KYPOST_KYCALENDAR_URL and KYPOST_KYCALENDAR_SECRET_FILE must be set together")
	}
	base, err := parseBase(rawURL)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("KYPOST_KYCALENDAR_SECRET_FILE cannot be read")
	}
	secret := bytes.TrimSpace(raw)
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("KYPOST_KYCALENDAR_SECRET_FILE must hold at least %d bytes", MinSecretBytes)
	}
	return New(base, secret, guardedTransport(allowPrivate)), nil
}

// parseBase accepts only https://host[:port], with no path, query or
// credentials, and never a loopback literal.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	bad := errors.New("KYPOST_KYCALENDAR_URL must be https://host with no path")
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, bad
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() || strings.EqualFold(u.Hostname(), "localhost") {
		return nil, errors.New("KYPOST_KYCALENDAR_URL must not be a loopback address")
	}
	u.Path = ""
	return u, nil
}

// New builds a Client paired with base and secret over rt. FromEnv passes the
// guarded transport; tests pass an httptest server's.
func New(base *url.URL, secret []byte, rt http.RoundTripper) *Client {
	c := newClient(rt)
	c.link.Store(&link{base: base, secret: secret, source: SourceEnv})
	return c
}

// NewKyIdentityForTest is New with the pairing marked as KyIdentity's, for
// other packages' tests of the per-user default. Panics outside a test binary.
func NewKyIdentityForTest(base *url.URL, secret []byte, rt http.RoundTripper) *Client {
	if !testing.Testing() {
		panic("kycalendar.NewKyIdentityForTest is test-only")
	}
	c := newClient(rt)
	c.link.Store(&link{base: base, secret: secret, source: SourceKyIdentity})
	return c
}

func newClient(rt http.RoundTripper) *Client {
	return &Client{
		http: &http.Client{
			Transport: &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes},
			Timeout:   30 * time.Second,
			// A redirect would replay the signed body somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		replay: newGuard(syncauth.DefaultWindow, 100000),
	}
}

// guardedTransport dials KyCalendar only at checked addresses: never
// loopback, unspecified, link-local or multicast, and other private or
// reserved ranges only with allowPrivate. The host is resolved once and the
// checked address dialled, so DNS cannot answer differently in between.
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

// Available reports whether KyCalendar is paired right now. Nil-safe.
func (c *Client) Available() bool { return c != nil && c.link.Load() != nil }

// DefaultOn is a user's KyCalendar switch when they have not chosen: on when
// KyIdentity paired KyCalendar, so the suite works without per-user setup;
// off when the environment did, as it always was there.
func (c *Client) DefaultOn() bool {
	if c == nil {
		return false
	}
	l := c.link.Load()
	return l != nil && l.source == SourceKyIdentity
}

// Post signs body as eventType/eventID and POSTs it to path on KyCalendar,
// returning the status and the response body. eventID must be stable across
// retries of the same message. A bad_signature answer asks Run to re-resolve
// the pairing: KyCalendar may hold a rotated secret.
func (c *Client) Post(ctx context.Context, path, eventType, eventID string, body any) (int, []byte, error) {
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
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && Outcome(out) == "bad_signature" {
		c.Refresh()
	}
	return resp.StatusCode, out, nil
}

// Outcome reads the "outcome" or "code" of a KyCalendar answer; anything
// but a short [a-z0-9_] token reads as "".
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

// Verify checks a request KyCalendar sent to the route for eventType. A
// signed request whose event ID was already accepted returns its event and an
// error wrapping syncauth.ErrReplay: answer 2xx without acting again. A bad
// signature asks Run to re-resolve the pairing, like Post's.
func (c *Client) Verify(r *http.Request, eventType string, body []byte) (syncauth.Event, error) {
	l := c.link.Load()
	if l == nil {
		return syncauth.Event{}, ErrOff
	}
	h := syncauth.FromRequest(r)
	if h.EventType != eventType {
		return syncauth.Event{}, errors.New("event type does not match the route")
	}
	ev, err := syncauth.Verify(l.secret, h, body, syncauth.Options{Replay: c.replay})
	if errors.Is(err, syncauth.ErrReplay) {
		ev = syncauth.Event{Type: h.EventType, ID: h.EventID}
	}
	if errors.Is(err, syncauth.ErrBadSignature) {
		c.Refresh()
	}
	return ev, err
}

// Forget lets the sender's retry of eventID run again: the attempt it names
// was refused or failed before anything was sent.
func (c *Client) Forget(eventID string) { c.replay.forget(eventID) }

// guard is syncauth's bounded in-memory replay guard plus forget. A restart
// forgets everything, which the signature's window bounds.
type guard struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	seen   map[string]time.Time
	order  []string
}

func newGuard(window time.Duration, max int) *guard {
	return &guard{window: window, max: max, seen: map[string]time.Time{}}
}

func (g *guard) Check(id string, at time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := time.Now().Add(-g.window)
	for len(g.order) > 0 {
		if t, ok := g.seen[g.order[0]]; ok && t.After(cutoff) {
			break
		} else if ok {
			delete(g.seen, g.order[0])
		}
		g.order = g.order[1:]
	}
	if _, ok := g.seen[id]; ok {
		return syncauth.ErrReplay
	}
	if len(g.order) >= g.max {
		return syncauth.ErrReplayGuardFull
	}
	g.seen[id] = at
	g.order = append(g.order, id)
	return nil
}

func (g *guard) forget(id string) {
	g.mu.Lock()
	delete(g.seen, id)
	g.mu.Unlock()
}
