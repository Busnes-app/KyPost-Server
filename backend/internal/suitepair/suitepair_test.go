package suitepair

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

var (
	testPeer = Peer{ID: "kymessage", Name: "KyMessages", Env: "KYPOST_KYMESSAGE"}
	secretA  = []byte(strings.Repeat("a", MinSecretBytes))
	secretB  = []byte(strings.Repeat("b", MinSecretBytes))
)

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(good, append([]byte("  "), append(secretA, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte(strings.Repeat("s", MinSecretBytes-1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		url, file, private string
		on, fails          bool
	}{
		"both unset":        {},
		"configured":        {url: "https://console.example.com", file: good, on: true},
		"trailing slash":    {url: "https://console.example.com/", file: good, on: true},
		"url only":          {url: "https://console.example.com", fails: true},
		"file only":         {file: good, fails: true},
		"http":              {url: "http://console.example.com", file: good, fails: true},
		"path":              {url: "https://console.example.com/x", file: good, fails: true},
		"query":             {url: "https://console.example.com?x=1", file: good, fails: true},
		"credentials":       {url: "https://u:p@console.example.com", file: good, fails: true},
		"loopback literal":  {url: "https://127.0.0.1", file: good, fails: true},
		"localhost":         {url: "https://localhost:8443", file: good, fails: true},
		"short secret":      {url: "https://console.example.com", file: short, fails: true},
		"missing secret":    {url: "https://console.example.com", file: filepath.Join(dir, "nope"), fails: true},
		"bad allow-private": {url: "https://console.example.com", file: good, private: "yes", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KYPOST_KYMESSAGE_URL", tc.url)
			t.Setenv("KYPOST_KYMESSAGE_SECRET_FILE", tc.file)
			t.Setenv("KYPOST_KYMESSAGE_ALLOW_PRIVATE", tc.private)
			c, err := FromEnv(testPeer)
			if (err != nil) != tc.fails || c.Available() != tc.on {
				t.Fatalf("FromEnv() = %v, %v; want on=%v fails=%v", c, err, tc.on, tc.fails)
			}
			if err != nil && (strings.Contains(err.Error(), string(secretA)) || !strings.Contains(err.Error(), "KYPOST_KYMESSAGE")) {
				t.Fatalf("error carries the secret or misses the variable: %v", err)
			}
			if tc.on && (string(c.link.Load().secret) != string(secretA) || c.Status() != (Status{Source: SourceEnv, Connected: true, URL: "https://console.example.com"})) {
				t.Fatalf("secret not trimmed or base not normalised: %+v", c.Status())
			}
		})
	}
}

// The guarded transport refuses a loopback server even with private allowed.
func TestGuardedTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(testPeer, base, secretA, guardedTransport(true))
	if _, _, err := c.Post(t.Context(), "/x", "ev.type", "id-1", map[string]string{}); !errors.Is(err, netguard.ErrInternalAddress) {
		t.Fatalf("Post to loopback: %v", err)
	}
}

// Post signs under the secret and event type, and never follows a redirect.
func TestPostSignsAndRefusesRedirects(t *testing.T) {
	var got []string
	redirected := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return
		case "/elsewhere":
			redirected = true
			return
		}
		body, _ := io.ReadAll(r.Body)
		ev, err := syncauth.Verify(secretA, syncauth.FromRequest(r), body, syncauth.Options{})
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Bad signature","code":"bad_signature"}`))
			return
		}
		got = append(got, r.URL.Path+" "+ev.Type+" "+ev.ID+" "+string(body))
		_, _ = w.Write([]byte(`{"url":"x"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(testPeer, base, secretA, srv.Client().Transport)
	if status, _, err := c.Post(t.Context(), "/api/suite/chat", "kypost.chat.open", "ev-1", map[string]string{"sub": "s"}); err != nil || status != 200 {
		t.Fatalf("Post = %d %v", status, err)
	}
	if want := `/api/suite/chat kypost.chat.open ev-1 {"sub":"s"}`; len(got) != 1 || got[0] != want {
		t.Fatalf("peer saw %q", got)
	}
	wrong := New(testPeer, base, secretB, srv.Client().Transport)
	if status, out, _ := wrong.Post(t.Context(), "/x", "kypost.chat.open", "ev-2", map[string]string{}); status != 401 || Outcome(out) != "bad_signature" {
		t.Fatalf("wrong secret answered %d %s", status, out)
	}
	if status, _, err := c.Post(t.Context(), "/redirect", "kypost.chat.open", "ev-3", map[string]string{}); err != nil || status != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("redirect followed: %d %v %v", status, err, redirected)
	}
	var unpaired *Client
	if _, _, err := unpaired.Post(t.Context(), "/x", "t", "id", nil); !errors.Is(err, ErrOff) {
		t.Fatalf("nil client Post = %v", err)
	}
}

func TestOutcome(t *testing.T) {
	for body, want := range map[string]string{
		`{"code":"no_target"}`:                       "no_target",
		`{"outcome":"duplicate"}`:                    "duplicate",
		`{"code":"No Target"}`:                       "",
		`{"code":"` + strings.Repeat("a", 65) + `"}`: "",
		`not json`: "",
	} {
		if got := Outcome([]byte(body)); got != want {
			t.Errorf("Outcome(%s) = %q", body, got)
		}
	}
}

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
		case r.URL.Path != "/api/suite/peer" || r.URL.Query().Get("client") != "kymessage":
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
			_ = json.NewEncoder(w).Encode(map[string]string{"client": "kymessage", "url": f.url, "secret": base64.StdEncoding.EncodeToString(f.secret)})
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

// fakePeer is the peer at https://example.com: it verifies requests under
// the secret the test sets and answers bad_signature otherwise.
type fakePeer struct {
	mu     sync.Mutex
	secret []byte
	srv    *httptest.Server
}

func newFakePeer(t *testing.T, secret []byte) *fakePeer {
	f := &fakePeer{secret: secret}
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
		_, _ = w.Write([]byte(`{"url":"https://chat.example.com/#/user/@bob:example.com"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// directoryClient is FromEnv's directory Client wired to the fakes: KyIdentity
// trusted, example.com dialled at the peer server, DNS answering ip.
func directoryClient(t *testing.T, id *fakeIdentity, peer *fakePeer, allowPrivate bool, ip string) *Client {
	t.Setenv("KYPOST_KYMESSAGE_URL", "")
	t.Setenv("KYPOST_KYMESSAGE_SECRET_FILE", "")
	t.Setenv("KYPOST_KYMESSAGE_ALLOW_PRIVATE", map[bool]string{true: "true"}[allowPrivate])
	c, err := FromEnv(testPeer)
	if err != nil || c.Available() || c.dir == nil {
		t.Fatalf("FromEnv with nothing set = %v, %v; want an unpaired directory client", c, err)
	}
	c.dir.http = id.srv.Client()
	c.dir.lookup = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP(ip)}, nil }
	if peer != nil {
		rt := peer.srv.Client().Transport.(*http.Transport).Clone()
		rt.TLSClientConfig.ServerName = "example.com"
		rt.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, peer.srv.Listener.Addr().String())
		}
		c.http.Transport = &netguard.BoundedTransport{Base: rt, Limit: maxResponseBytes}
	}
	return c
}

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
		"private, refused":       {status: 200, ip: "192.168.1.20", reason: "KYPOST_KYMESSAGE_ALLOW_PRIVATE"},
		"loopback, even allowed": {status: 200, ip: "127.0.0.1", allowPrivate: true, reason: "never allowed"},
		"loopback url":           {status: 200, url: "https://localhost", ip: "8.8.8.8", reason: "unusable KyMessages"},
		"url with path":          {status: 200, url: "https://example.com/c", ip: "8.8.8.8", reason: "unusable"},
		"no peer":                {status: 404, ip: "8.8.8.8", reason: "no KyMessages"},
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
			if tc.on && (st.Source != SourceKyIdentity || st.URL != "https://example.com" || string(c.link.Load().secret) != string(secretA)) {
				t.Fatalf("paired link wrong: %+v", st)
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

// Run pairs at start, follows a secret rotation after the peer answers
// bad_signature, and unpairs when SSO is turned off, with no restart.
func TestRunRotationAndSettingsChange(t *testing.T) {
	previous := settingsPoll
	settingsPoll = 20 * time.Millisecond
	t.Cleanup(func() { settingsPoll = previous })
	id := newFakeIdentity(t, secretA)
	peer := newFakePeer(t, secretA)
	c := directoryClient(t, id, peer, false, "8.8.8.8")
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
		status, _, _ := c.Post(t.Context(), "/api/suite/chat", "kypost.chat.open", "ev-"+time.Now().String(), map[string]string{})
		return status
	}
	eventually(t, "paired at start", func() bool { return post() == http.StatusOK })

	id.set(http.StatusOK, secretB)
	peer.mu.Lock()
	peer.secret = secretB
	peer.mu.Unlock()
	if post() != http.StatusUnauthorized {
		t.Fatal("rotated peer accepted the old secret")
	}
	eventually(t, "rotation followed", func() bool { return post() == http.StatusOK })

	mu.Lock()
	dir = Directory{}
	mu.Unlock()
	eventually(t, "unpaired when SSO is off", func() bool { return !c.Available() })
	if _, _, err := c.Post(t.Context(), "/x", "kypost.chat.open", "ev", nil); !errors.Is(err, ErrOff) {
		t.Fatalf("Post while off = %v", err)
	}
	cancel()
	<-done
	out := logs.String()
	if !strings.Contains(out, "kymessage pairing through kyidentity") {
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
	c := newDirectoryClient(testPeer, false)
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
	New(testPeer, nil, secretA, nil).Refresh() // an environment pairing ignores it
}

// The environment wins entirely: Run never asks KyIdentity.
func TestEnvironmentWins(t *testing.T) {
	id := newFakeIdentity(t, secretA)
	base, _ := testPeer.parseBase("https://console.example.com")
	c := New(testPeer, base, secretA, nil)
	c.Run(t.Context(), id.directory, nil)
	if id.calls != 0 || c.Status() != (Status{Source: SourceEnv, Connected: true, URL: "https://console.example.com"}) {
		t.Fatalf("env client consulted KyIdentity (%d) or reports %+v", id.calls, c.Status())
	}
	var nilClient *Client
	if nilClient.Available() || nilClient.Status().Connected {
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

// Verify accepts a signed request once: a second delivery of the same event
// ID is a replay until forgotten; a wrong event type or secret is refused.
func TestVerifyReplayForget(t *testing.T) {
	c := New(testPeer, nil, secretA, nil)
	verify := func(secret []byte, eventType, id string) error {
		body := []byte(`{"a":"b"}`)
		h, err := syncauth.Sign(secret, time.Now(), eventType, id, body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		h.Apply(r)
		_, err = c.Verify(r, "peer.event", body)
		return err
	}
	if err := verify(secretA, "peer.event", "ev-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := verify(secretA, "peer.event", "ev-1"); !errors.Is(err, syncauth.ErrReplay) {
		t.Fatalf("replay: %v", err)
	}
	c.Forget("ev-1")
	if err := verify(secretA, "peer.event", "ev-1"); err != nil {
		t.Fatalf("after forget: %v", err)
	}
	if err := verify(secretA, "other.event", "ev-2"); err == nil {
		t.Fatal("wrong event type accepted")
	}
	if err := verify(secretB, "peer.event", "ev-3"); !errors.Is(err, syncauth.ErrBadSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	var off *Client
	if _, err := off.Verify(httptest.NewRequest(http.MethodPost, "/x", nil), "peer.event", nil); !errors.Is(err, ErrOff) {
		t.Fatalf("nil client: %v", err)
	}
	if FromKyIdentity(testPeer).Available() {
		t.Fatal("a KyIdentity-only client is paired before Run")
	}
}

func TestGuardExpiresAndBounds(t *testing.T) {
	g := newGuard(time.Minute, 2)
	now := time.Now()
	if g.Check("a", now) != nil || g.Check("b", now) != nil {
		t.Fatal("fresh IDs refused")
	}
	if !errors.Is(g.Check("c", now), syncauth.ErrReplayGuardFull) {
		t.Fatal("full guard accepted a new ID")
	}
	g2 := newGuard(time.Minute, 1)
	_ = g2.Check("old", now.Add(-2*time.Minute))
	if g2.Check("new", now) != nil {
		t.Fatal("expired ID still held its slot")
	}
}
