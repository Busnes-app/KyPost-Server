package kycalendar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

// fakeIdentity is KyIdentity's suite directory: it answers /api/suite/peer
// for KyPost's client credentials with whatever the test sets.
type fakeIdentity struct {
	mu     sync.Mutex
	status int
	url    string
	secret []byte
	calls  int
	srv    *httptest.Server
}

func newFakeIdentity(t *testing.T, secret []byte) *fakeIdentity {
	f := &fakeIdentity{status: 200, url: "https://example.com", secret: secret}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		id, pw, ok := r.BasicAuth()
		switch {
		case r.URL.Path != "/api/suite/peer" || r.URL.Query().Get("client") != "kycalendar":
			w.WriteHeader(http.StatusBadRequest)
		case !ok || id != "kypost" || pw != "client-secret":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		case f.status == http.StatusNotFound:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no_peer"}`))
		case f.status != http.StatusOK:
			w.WriteHeader(f.status)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"client": "kycalendar", "url": f.url, "secret": base64.StdEncoding.EncodeToString(f.secret)})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdentity) set(status int, secret []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.secret = status, secret
}

func (f *fakeIdentity) directory() Directory {
	return Directory{Issuer: f.srv.URL + "/", ClientID: "kypost", ClientSecret: "client-secret"}
}

// fakeCalendar is KyCalendar at https://example.com: it verifies requests
// under the secret the test sets and answers bad_signature otherwise.
type fakeCalendar struct {
	mu     sync.Mutex
	secret []byte
	srv    *httptest.Server
}

func newFakeCalendar(t *testing.T, secret []byte) *fakeCalendar {
	f := &fakeCalendar{secret: secret}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		secret := f.secret
		f.mu.Unlock()
		if _, err := syncauth.Verify(secret, syncauth.FromRequest(r), body, syncauth.Options{}); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Bad signature","code":"bad_signature"}`))
			return
		}
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// directoryClient is FromEnv's directory Client wired to the fakes: KyIdentity
// trusted, example.com dialled at the calendar server, DNS answering ip.
func directoryClient(t *testing.T, id *fakeIdentity, cal *fakeCalendar, allowPrivate bool, ip string) *Client {
	t.Setenv("KYPOST_KYCALENDAR_URL", "")
	t.Setenv("KYPOST_KYCALENDAR_SECRET_FILE", "")
	t.Setenv("KYPOST_KYCALENDAR_ALLOW_PRIVATE", map[bool]string{true: "true"}[allowPrivate])
	c, err := FromEnv()
	if err != nil || c.Available() || c.dir == nil {
		t.Fatalf("FromEnv with nothing set = %v, %v; want an unpaired directory client", c, err)
	}
	c.dir.http = id.srv.Client()
	c.dir.lookup = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP(ip)}, nil }
	if cal != nil {
		rt := cal.srv.Client().Transport.(*http.Transport).Clone()
		rt.TLSClientConfig.ServerName = "example.com"
		rt.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, cal.srv.Listener.Addr().String())
		}
		c.http.Transport = &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes}
	}
	return c
}

var (
	secretA = []byte(strings.Repeat("a", 32))
	secretB = []byte(strings.Repeat("b", 32))
)

func TestResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		url          string
		ip           string
		allowPrivate bool
		noSSO        bool
		badCreds     bool
		on           bool
		reason       string
	}{
		"paired":                 {status: 200, ip: "8.8.8.8", on: true, reason: "Connected"},
		"private, allowed":       {status: 200, ip: "192.168.1.20", allowPrivate: true, on: true},
		"private, refused":       {status: 200, ip: "192.168.1.20", reason: "KYPOST_KYCALENDAR_ALLOW_PRIVATE"},
		"loopback, even allowed": {status: 200, ip: "127.0.0.1", allowPrivate: true, reason: "never allowed"},
		"loopback url":           {status: 200, url: "https://localhost", ip: "8.8.8.8", reason: "unusable"},
		"url with path":          {status: 200, url: "https://example.com/cal", ip: "8.8.8.8", reason: "unusable"},
		"no peer":                {status: 404, ip: "8.8.8.8", reason: "no KyCalendar"},
		"bad credentials":        {status: 200, badCreds: true, ip: "8.8.8.8", reason: "client credentials"},
		"identity down":          {status: 503, ip: "8.8.8.8", reason: "could not be reached"},
		"sso not set up":         {noSSO: true, reason: "does not sign in"},
	} {
		t.Run(name, func(t *testing.T) {
			id := newFakeIdentity(t, secretA)
			id.status = tc.status
			if tc.url != "" {
				id.url = tc.url
			}
			c := directoryClient(t, id, nil, tc.allowPrivate, tc.ip)
			d := id.directory()
			if tc.badCreds {
				d.ClientSecret = "wrong"
			}
			if tc.noSSO {
				d = Directory{}
			}
			err := c.resolve(t.Context(), d)
			st := c.Status()
			if c.Available() != tc.on || st.Connected != tc.on || (err == nil) != (tc.on || tc.noSSO) || !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("resolve = %v, status %+v; want on=%v reason~%q", err, st, tc.on, tc.reason)
			}
			if tc.on && (!c.DefaultOn() || st.Source != SourceKyIdentity || st.URL != "https://example.com" ||
				string(c.link.Load().secret) != string(secretA)) {
				t.Fatalf("paired link wrong: %+v default=%v", st, c.DefaultOn())
			}
			raw, _ := json.Marshal(st)
			if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(secretA)) || strings.Contains(string(raw), "client-secret") ||
				err != nil && strings.Contains(err.Error(), "client-secret") {
				t.Fatal("status or error carries a secret")
			}
		})
	}
}

// A KyIdentity outage keeps a pairing those same settings produced; a
// definitive refusal or different settings turn it off.
func TestResolveKeepsLinkThroughOutage(t *testing.T) {
	id := newFakeIdentity(t, secretA)
	c := directoryClient(t, id, nil, false, "8.8.8.8")
	if err := c.resolve(t.Context(), id.directory()); err != nil {
		t.Fatal(err)
	}
	id.set(http.StatusBadGateway, nil)
	if err := c.resolve(t.Context(), id.directory()); err == nil || !c.Available() || !strings.Contains(c.Status().Reason, "keeping") {
		t.Fatalf("outage dropped the link: %v %+v", err, c.Status())
	}
	other := id.directory()
	other.ClientID = "kypost-2"
	if err := c.resolve(t.Context(), other); err == nil || c.Available() {
		t.Fatalf("changed settings kept the old link: %v %+v", err, c.Status())
	}
	id.set(http.StatusOK, secretA)
	_ = c.resolve(t.Context(), id.directory())
	id.set(http.StatusNotFound, nil)
	if err := c.resolve(t.Context(), id.directory()); !errors.Is(err, suitedir.ErrNoPeer) || c.Available() {
		t.Fatalf("no_peer kept the link: %v %+v", err, c.Status())
	}
}

// Run pairs at start, follows a secret rotation after KyCalendar answers
// bad_signature, and unpairs when SSO is turned off, with no restart.
func TestRunRotationAndSettingsChange(t *testing.T) {
	previous := settingsPoll
	settingsPoll = 20 * time.Millisecond
	t.Cleanup(func() { settingsPoll = previous })
	id := newFakeIdentity(t, secretA)
	cal := newFakeCalendar(t, secretA)
	c := directoryClient(t, id, cal, false, "8.8.8.8")
	var mu sync.Mutex
	dir := id.directory()
	settings := func() Directory { mu.Lock(); defer mu.Unlock(); return dir }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var logs syncBuffer
	log, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	go func() { c.Run(ctx, settings, log); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	post := func() int {
		status, _, _ := c.Post(t.Context(), "/api/itip/inbound", EventInbound, "ev-"+time.Now().String(), map[string]string{})
		return status
	}
	eventually(t, "paired at start", func() bool { return post() == http.StatusOK })

	id.set(http.StatusOK, secretB)
	cal.mu.Lock()
	cal.secret = secretB
	cal.mu.Unlock()
	if post() != http.StatusUnauthorized {
		t.Fatal("rotated KyCalendar accepted the old secret")
	}
	eventually(t, "rotation followed", func() bool { return post() == http.StatusOK })

	mu.Lock()
	dir = Directory{}
	mu.Unlock()
	eventually(t, "unpaired when SSO is off", func() bool { return !c.Available() })
	if _, _, err := c.Post(t.Context(), "/x", EventInbound, "ev", nil); !errors.Is(err, ErrOff) {
		t.Fatalf("Post while off = %v", err)
	}
	cancel()
	<-done
	out := logs.String()
	if !strings.Contains(out, "kycalendar pairing through kyidentity") {
		t.Fatalf("no pairing log: %s", out)
	}
	for _, secret := range []string{string(secretA), string(secretB), base64.StdEncoding.EncodeToString(secretB), "client-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log carries a secret: %s", out)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRefreshIsRateLimited(t *testing.T) {
	c := newDirectoryClient(false)
	c.Refresh()
	c.Refresh()
	if len(c.dir.kick) != 1 {
		t.Fatalf("kicks = %d", len(c.dir.kick))
	}
	<-c.dir.kick
	c.Refresh()
	if len(c.dir.kick) != 0 {
		t.Fatal("a second refresh within the gap kicked again")
	}
	New(nil, testSecret, nil).Refresh() // an environment pairing ignores it
}

// The environment wins entirely: Run never asks KyIdentity.
func TestEnvironmentWins(t *testing.T) {
	id := newFakeIdentity(t, secretA)
	base, _ := parseBase("https://cal.example.com")
	c := New(base, testSecret, nil)
	c.Run(t.Context(), id.directory, nil)
	if id.calls != 0 || c.DefaultOn() || c.Status() != (Status{Source: SourceEnv, Connected: true, URL: "https://cal.example.com"}) {
		t.Fatalf("env client consulted KyIdentity (%d) or reports %+v", id.calls, c.Status())
	}
	var nilClient *Client
	if nilClient.Available() || nilClient.DefaultOn() || nilClient.Status().Connected {
		t.Fatal("nil client reads as paired")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(what + ": timed out")
}
