package kycalendar

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// Where a pairing came from.
const (
	SourceEnv        = "env"
	SourceKyIdentity = "kyidentity"
)

// Run's timing: how often it re-resolves a working pairing and retries a
// failed one, and how often a bad signature may trigger a re-resolve (an
// unsigned request can cause one).
const (
	resolveEvery = 15 * time.Minute
	retryMin     = 30 * time.Second
	retryMax     = 15 * time.Minute
	refreshGap   = time.Minute
)

// settingsPoll is how often Run rereads the SSO settings; a var for tests.
var settingsPoll = 15 * time.Second

// Directory is KyPost's own OAuth client at KyIdentity, which the suite
// directory authenticates. Zero when KyPost does not sign in through SSO.
type Directory struct {
	Issuer, ClientID, ClientSecret string
}

// Status is the pairing as an administrator sees it. Never the secret.
type Status struct {
	Source    string `json:"source,omitempty"`
	Connected bool   `json:"connected"`
	URL       string `json:"url,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type directory struct {
	allowPrivate bool
	http         *http.Client // nil: suitedir's own; tests replace it
	lookup       func(ctx context.Context, network, host string) ([]net.IP, error)
	kick         chan struct{}
	lastKick     atomic.Int64

	mu     sync.Mutex
	status Status
	from   Directory // the settings the current link was resolved with
}

func newDirectoryClient(allowPrivate bool) *Client {
	c := newClient(guardedTransport(allowPrivate))
	c.dir = &directory{
		allowPrivate: allowPrivate,
		lookup:       net.DefaultResolver.LookupIP,
		kick:         make(chan struct{}, 1),
		status:       Status{Reason: "Not checked yet"},
	}
	return c
}

// Status reports the current pairing. Nil-safe.
func (c *Client) Status() Status {
	if c == nil {
		return Status{Reason: "KyCalendar is not configured"}
	}
	if c.dir == nil {
		return Status{Source: SourceEnv, Connected: true, URL: c.link.Load().base.String()}
	}
	c.dir.mu.Lock()
	defer c.dir.mu.Unlock()
	return c.dir.status
}

// Refresh asks Run to re-resolve now, at most once per refreshGap. A no-op
// for an environment pairing.
func (c *Client) Refresh() {
	if c.dir == nil {
		return
	}
	now, last := time.Now().UnixNano(), c.dir.lastKick.Load()
	if now-last < int64(refreshGap) || !c.dir.lastKick.CompareAndSwap(last, now) {
		return
	}
	select {
	case c.dir.kick <- struct{}{}:
	default:
	}
}

// Run keeps a KyIdentity pairing current until ctx ends: at once, every
// resolveEvery, within settingsPoll of an SSO settings change, and after
// Refresh. A failure is retried with backoff. Returns at once for an
// environment pairing.
func (c *Client) Run(ctx context.Context, settings func() Directory, log *logging.Logger) {
	if c == nil || c.dir == nil {
		return
	}
	poll := time.NewTicker(settingsPoll)
	defer poll.Stop()
	var last Directory
	var next time.Time
	retry, first := retryMin, true
	for {
		if d := settings(); first || d != last || !time.Now().Before(next) {
			first, last = false, d
			was := c.Status()
			err := c.resolve(ctx, d)
			now := c.Status()
			if err != nil {
				next, retry = time.Now().Add(retry), min(retry*2, retryMax)
				log.Warn("kycalendar pairing through kyidentity failed", "reason", now.Reason, "error", err.Error())
			} else {
				next, retry = time.Now().Add(resolveEvery), retryMin
				if now != was {
					log.Info("kycalendar pairing through kyidentity", "result", now.Reason, "base_url", now.URL)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-c.dir.kick:
			next = time.Time{}
		}
	}
}

// resolve asks KyIdentity for KyCalendar and installs the answer. Every
// failure turns the link off, except a transient one (KyIdentity unreachable
// or erroring) while the link came from these same settings: an outage of
// KyIdentity alone does not unpair a working KyCalendar.
func (c *Client) resolve(ctx context.Context, d Directory) error {
	if d == (Directory{}) {
		c.set(nil, Status{Reason: "KyPost does not sign in through KyIdentity"}, d)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	off := func(reason string, err error) error {
		c.set(nil, Status{Source: SourceKyIdentity, Reason: reason}, d)
		return err
	}
	peer, err := (&suitedir.Client{Issuer: d.Issuer, ClientID: d.ClientID, ClientSecret: d.ClientSecret, HTTP: c.dir.http}).Peer(ctx, "kycalendar")
	switch {
	case errors.Is(err, suitedir.ErrNoPeer):
		return off("KyIdentity has no KyCalendar to pair with", err)
	case errors.Is(err, suitedir.ErrUnauthorized):
		return off("KyIdentity refused KyPost's client credentials", err)
	case errors.Is(err, suitedir.ErrInsecureIssuer):
		return off("KyPost's sign-in issuer is not https", err)
	case err != nil:
		// Unreachable, a 5xx or a malformed answer.
		c.dir.mu.Lock()
		keep := c.link.Load() != nil && c.dir.from == d
		if keep {
			c.dir.status.Reason = "KyIdentity could not be reached; keeping the last pairing"
		}
		c.dir.mu.Unlock()
		if keep {
			return err
		}
		return off("KyIdentity could not be reached or gave a bad answer", err)
	}
	base, err := parseBase(peer.URL)
	if err != nil {
		return off("KyIdentity gave an unusable KyCalendar address", err)
	}
	ips, err := c.dir.lookup(ctx, "ip", base.Hostname())
	if err != nil {
		return off("KyCalendar's host name does not resolve", err)
	}
	for _, ip := range ips {
		if refused(ip, true) {
			return off("KyCalendar is at a loopback or link-local address, which is never allowed", netguard.ErrInternalAddress)
		}
		if refused(ip, c.dir.allowPrivate) {
			return off("KyCalendar is at a private address; set KYPOST_KYCALENDAR_ALLOW_PRIVATE=true to allow it", netguard.ErrInternalAddress)
		}
	}
	c.set(&link{base: base, secret: peer.Secret, source: SourceKyIdentity},
		Status{Source: SourceKyIdentity, Connected: true, URL: base.String(), Reason: "Connected"}, d)
	return nil
}

func (c *Client) set(l *link, st Status, from Directory) {
	c.dir.mu.Lock()
	defer c.dir.mu.Unlock()
	c.dir.status, c.dir.from = st, from
	c.link.Store(l)
}
