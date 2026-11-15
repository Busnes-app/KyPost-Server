// Package kydrive is KyPost's half of the KyDrive files contract
// (docs/KYDRIVE.md): the pairing, the signed client for KyPost → KyDrive
// requests, and the server-side upload of a received attachment to a ticket.
// KyDrive never calls KyPost, so there is no receiver here.
//
// ponytail: the pairing half (FromEnv, guardedTransport, directory.go) is
// internal/kycalendar's with the names changed. Fold both into one suite-pair
// package once a third peer needs it.
package kydrive

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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// Event types, one per route.
const (
	EventTicket = "kypost.drive.ticket"
	EventLink   = "kypost.drive.link"
	EventStatus = "kypost.drive.status"
	EventFetch  = "kypost.drive.fetch"
)

// Routes on KyDrive.
const (
	PathTicket = "/api/suite/kypost/upload-tickets"
	PathLink   = "/api/suite/kypost/links"
	PathStatus = "/api/suite/kypost/status"
	PathFetch  = "/api/suite/kypost/fetch"
	// UploadPrefix is where every ticket URL must point.
	UploadPrefix = "/api/suite/upload/"
	// LinkPrefix is where every link URL must point.
	LinkPrefix = "/l/"
)

// MinSecretBytes is the shortest shared secret accepted, after trimming.
const MinSecretBytes = 32

// maxResponseBytes caps a KyDrive answer; they are a few bytes of JSON.
const maxResponseBytes = 64 << 10

// MaxFetchBytes caps a fetched file's answer; callers name a lower max_bytes.
const MaxFetchBytes = 32 << 20

// ErrOff is a request made while KyDrive is not paired.
var ErrOff = errors.New("kydrive: not paired")

// Client signs requests to KyDrive. It is paired by the environment for the
// life of the process, or through KyIdentity by Run.
type Client struct {
	link   atomic.Pointer[link]
	http   *http.Client
	upload *http.Client
	fetch  *http.Client
	dir    *directory // nil when the environment paired it
}

type link struct {
	base   *url.URL
	secret []byte
	source string
}

// FromEnv reads KYPOST_KYDRIVE_URL, KYPOST_KYDRIVE_SECRET_FILE and
// KYPOST_KYDRIVE_ALLOW_PRIVATE. Both unset returns a Client that Run pairs
// through KyIdentity; anything else unusable is an error.
func FromEnv() (*Client, error) {
	rawURL := strings.TrimSpace(os.Getenv("KYPOST_KYDRIVE_URL"))
	file := strings.TrimSpace(os.Getenv("KYPOST_KYDRIVE_SECRET_FILE"))
	allowPrivate, err := config.EnvBool("KYPOST_KYDRIVE_ALLOW_PRIVATE")
	if err != nil {
		return nil, err
	}
	if rawURL == "" && file == "" {
		return newDirectoryClient(allowPrivate), nil
	}
	if rawURL == "" || file == "" {
		return nil, errors.New("KYPOST_KYDRIVE_URL and KYPOST_KYDRIVE_SECRET_FILE must be set together")
	}
	base, err := parseBase(rawURL)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("KYPOST_KYDRIVE_SECRET_FILE cannot be read")
	}
	secret := bytes.TrimSpace(raw)
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("KYPOST_KYDRIVE_SECRET_FILE must hold at least %d bytes", MinSecretBytes)
	}
	return New(base, secret, guardedTransport(allowPrivate)), nil
}

// parseBase accepts only https://host[:port], never a loopback literal.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	bad := errors.New("KYPOST_KYDRIVE_URL must be https://host with no path")
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, bad
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() || strings.EqualFold(u.Hostname(), "localhost") {
		return nil, errors.New("KYPOST_KYDRIVE_URL must not be a loopback address")
	}
	u.Path = ""
	return u, nil
}

// New builds a Client paired with base and secret over rt. Tests pass an
// httptest server's transport.
func New(base *url.URL, secret []byte, rt http.RoundTripper) *Client {
	c := newClient(rt)
	c.link.Store(&link{base: base, secret: secret, source: SourceEnv})
	return c
}

func newClient(rt http.RoundTripper) *Client {
	// A redirect would replay the signed body, or the ticket, somewhere else.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		http: &http.Client{
			Transport:     &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes},
			Timeout:       30 * time.Second,
			CheckRedirect: noRedirect,
		},
		// An upload or fetch is bounded by its caller's context, not a fixed timeout.
		upload: &http.Client{
			Transport:     &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes},
			CheckRedirect: noRedirect,
		},
		fetch: &http.Client{
			Transport:     &netguard.BoundedTransport{Base: rt, Limit: MaxFetchBytes},
			CheckRedirect: noRedirect,
		},
	}
}

// guardedTransport dials KyDrive only at checked addresses: never loopback,
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
		ResponseHeaderTimeout: 60 * time.Second,
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

// Available reports whether KyDrive is paired right now. Nil-safe.
func (c *Client) Available() bool { return c != nil && c.link.Load() != nil }

// Origin is the paired KyDrive's https://host[:port], or "" unpaired. Nil-safe.
func (c *Client) Origin() string {
	if c == nil {
		return ""
	}
	if l := c.link.Load(); l != nil {
		return l.base.String()
	}
	return ""
}

// OnOrigin reports whether raw is an https URL on the paired KyDrive, with no
// credentials, whose path starts with prefix and names something after it in
// the path or the fragment (a link's token is its fragment). KyDrive's answers
// are checked with it before a URL reaches the browser or the uploader.
func (c *Client) OnOrigin(raw, prefix string) bool {
	origin := c.Origin()
	u, err := url.Parse(raw)
	return origin != "" && err == nil && u.Scheme == "https" && u.User == nil && u.Opaque == "" &&
		(&url.URL{Scheme: u.Scheme, Host: u.Host}).String() == origin &&
		strings.HasPrefix(u.Path, prefix) && (len(u.Path) > len(prefix) || u.Fragment != "") && !strings.Contains(u.Path, "..")
}

// Post signs body as eventType/eventID and POSTs it to path on KyDrive,
// returning the status and the response body. A bad_signature answer asks
// Run to re-resolve the pairing.
func (c *Client) Post(ctx context.Context, path, eventType, eventID string, body any) (int, []byte, error) {
	status, _, out, err := c.post(ctx, c.http, path, eventType, eventID, body)
	return status, out, err
}

// Fetch is kypost.drive.fetch: the answer is the file itself (at most
// MaxFetchBytes are read; a longer one is truncated), returned with its
// headers so the caller can check the type and read the name.
func (c *Client) Fetch(ctx context.Context, eventID string, body any) (int, http.Header, []byte, error) {
	return c.post(ctx, c.fetch, PathFetch, EventFetch, eventID, body)
}

func (c *Client) post(ctx context.Context, hc *http.Client, path, eventType, eventID string, body any) (int, http.Header, []byte, error) {
	l := c.link.Load()
	if l == nil {
		return 0, nil, nil, ErrOff
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, err
	}
	h, err := syncauth.Sign(l.secret, time.Now(), eventType, eventID, payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base.JoinPath(path).String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	h.Apply(req)
	req.Header.Set("Content-Type", "application/json")
	status, header, out, err := c.do(hc, req)
	if status == http.StatusUnauthorized && Outcome(out) == "bad_signature" {
		c.Refresh()
	}
	return status, header, out, err
}

// Upload PUTs size bytes from r to a ticket URL, in Content-Range chunks of
// at most maxChunk when larger, and returns KyDrive's last answer. The URL is
// the credential: it must be on the paired origin, and errors never carry it.
func (c *Client) Upload(ctx context.Context, ticketURL string, r io.ReaderAt, size, maxChunk int64) (int, []byte, error) {
	if !c.OnOrigin(ticketURL, UploadPrefix) {
		return 0, nil, errors.New("kydrive: ticket URL is not on the paired KyDrive")
	}
	if maxChunk <= 0 || maxChunk > size {
		maxChunk = size
	}
	for off := int64(0); ; off += maxChunk {
		n := min(maxChunk, size-off)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, ticketURL, io.NewSectionReader(r, off, n))
		if err != nil {
			return 0, nil, errors.New("kydrive: bad ticket URL")
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", "application/octet-stream")
		if n < size {
			req.Header.Set("Content-Range", "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10)+"/"+strconv.FormatInt(size, 10))
		}
		status, _, out, err := c.do(c.upload, req)
		if err != nil || off+n >= size || status/100 != 2 {
			return status, out, err
		}
	}
}

func (c *Client) do(hc *http.Client, req *http.Request) (int, http.Header, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error carries the URL, and a ticket URL is a credential.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, nil, nil, fmt.Errorf("kydrive: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("kydrive: read answer: %w", err)
	}
	return resp.StatusCode, resp.Header, out, nil
}

// Outcome reads the "outcome" or "code" of a KyDrive answer; anything but a
// short [a-z0-9_] token reads as "".
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
