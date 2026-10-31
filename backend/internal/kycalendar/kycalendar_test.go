package kycalendar

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/netguard"
)

var testSecret = []byte(strings.Repeat("s", MinSecretBytes))

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(good, append([]byte("  "), append(testSecret, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte(" "+strings.Repeat("s", MinSecretBytes-1)+" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		url, file, private string
		on, fails          bool
	}{
		"both unset":        {},
		"configured":        {url: "https://cal.example.com", file: good, on: true},
		"trailing slash":    {url: "https://cal.example.com/", file: good, on: true},
		"url only":          {url: "https://cal.example.com", fails: true},
		"file only":         {file: good, fails: true},
		"http":              {url: "http://cal.example.com", file: good, fails: true},
		"path":              {url: "https://cal.example.com/kycal", file: good, fails: true},
		"query":             {url: "https://cal.example.com?x=1", file: good, fails: true},
		"credentials":       {url: "https://u:p@cal.example.com", file: good, fails: true},
		"loopback literal":  {url: "https://127.0.0.1", file: good, fails: true},
		"localhost":         {url: "https://localhost:8443", file: good, fails: true},
		"short secret":      {url: "https://cal.example.com", file: short, fails: true},
		"missing secret":    {url: "https://cal.example.com", file: filepath.Join(dir, "nope"), fails: true},
		"bad allow-private": {url: "https://cal.example.com", file: good, private: "yes", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KYPOST_KYCALENDAR_URL", tc.url)
			t.Setenv("KYPOST_KYCALENDAR_SECRET_FILE", tc.file)
			t.Setenv("KYPOST_KYCALENDAR_ALLOW_PRIVATE", tc.private)
			c, err := FromEnv()
			if (err != nil) != tc.fails || c.Available() != tc.on || (err == nil) != (c != nil) {
				t.Fatalf("FromEnv() = %v, %v; want on=%v fails=%v", c, err, tc.on, tc.fails)
			}
			if err != nil && strings.Contains(err.Error(), string(testSecret)) {
				t.Fatal("error carries the secret")
			}
			if c.Available() {
				if l := c.link.Load(); string(l.secret) != string(testSecret) || l.base.String() != "https://cal.example.com" || c.DefaultOn() {
					t.Fatalf("secret not trimmed, base not normalised or default on: %q %s", l.secret, l.base)
				}
			}
		})
	}
}

func TestRefusedAddresses(t *testing.T) {
	for _, tc := range []struct {
		ip                 string
		strict, lanAllowed bool
	}{
		{"127.0.0.1", true, true},
		{"::1", true, true},
		{"0.0.0.0", true, true},
		{"169.254.169.254", true, true},
		{"10.0.0.5", true, false},
		{"192.168.1.20", true, false},
		{"100.64.1.1", true, false},
		{"203.0.113.9", true, false}, // TEST-NET-3 is reserved
		{"8.8.8.8", false, false},
	} {
		ip := net.ParseIP(tc.ip)
		if got := refused(ip, false); got != tc.strict {
			t.Errorf("refused(%s, false) = %v", tc.ip, got)
		}
		if got := refused(ip, true); got != tc.lanAllowed {
			t.Errorf("refused(%s, true) = %v", tc.ip, got)
		}
	}
}

// The guarded transport refuses a loopback server even with private allowed.
func TestGuardedTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, testSecret, guardedTransport(true))
	if _, _, err := c.Post(t.Context(), "/api/itip/inbound", EventInbound, "id-1", map[string]string{}); !errors.Is(err, netguard.ErrInternalAddress) {
		t.Fatalf("Post to loopback: %v", err)
	}
}

// Post signs what it sends, never follows a redirect, and Verify accepts it
// once: a second delivery of the same event ID is a replay until forgotten.
func TestPostVerifyReplayForget(t *testing.T) {
	redirected := false
	var receiver *Client
	var results []error
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		_, err := receiver.Verify(r, EventInbound, body)
		results = append(results, err)
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	sender := New(base, testSecret, srv.Client().Transport)
	receiver = New(base, testSecret, srv.Client().Transport)

	status, answer, err := sender.Post(t.Context(), "/api/itip/inbound", EventInbound, "ev-1", map[string]string{"a": "b"})
	if err != nil || status != 200 || Outcome(answer) != "applied" {
		t.Fatalf("Post = %d %q %v", status, answer, err)
	}
	_, _, _ = sender.Post(t.Context(), "/api/itip/inbound", EventInbound, "ev-1", map[string]string{"a": "b"})
	receiver.Forget("ev-1")
	_, _, _ = sender.Post(t.Context(), "/api/itip/inbound", EventInbound, "ev-1", map[string]string{"a": "b"})
	_, _, _ = sender.Post(t.Context(), "/api/itip/inbound", EventRespond, "ev-2", map[string]string{"a": "b"})
	other := New(base, []byte(strings.Repeat("x", MinSecretBytes)), srv.Client().Transport)
	_, _, _ = other.Post(t.Context(), "/api/itip/inbound", EventInbound, "ev-3", map[string]string{"a": "b"})
	if len(results) != 5 || results[0] != nil || !errors.Is(results[1], syncauth.ErrReplay) || results[2] != nil ||
		results[3] == nil || !errors.Is(results[4], syncauth.ErrBadSignature) {
		t.Fatalf("verify results = %v", results)
	}
	if status, _, err := sender.Post(t.Context(), "/redirect", EventInbound, "ev-4", map[string]string{}); err != nil || status != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("redirect followed: %d %v %v", status, err, redirected)
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
	g.forget("a")
	if g.Check("a", now) != nil || !errors.Is(g.Check("a", now), syncauth.ErrReplay) {
		t.Fatal("forgotten ID not accepted exactly once")
	}
	g2 := newGuard(time.Minute, 1)
	_ = g2.Check("old", now.Add(-2*time.Minute))
	if g2.Check("new", now) != nil {
		t.Fatal("expired ID still held its slot")
	}
}

func TestInvitations(t *testing.T) {
	s := OpenInvitations(filepath.Join(t.TempDir(), "users", "u1"))
	if ok, err := s.Has("e1", "o@x.test"); ok || err != nil {
		t.Fatalf("empty record: %v %v", ok, err)
	}
	if err := s.Record("e1", "O@X.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("e1", "o@x.test"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uid, org string
		want     bool
	}{{"e1", "o@x.test", true}, {"e1", "O@x.TEST", true}, {"E1", "o@x.test", false}, {"e1", "p@x.test", false}} {
		if ok, err := s.Has(tc.uid, tc.org); ok != tc.want || err != nil {
			t.Errorf("Has(%q, %q) = %v %v", tc.uid, tc.org, ok, err)
		}
	}
	full := make([]invitation, maxInvitations)
	full[0] = invitation{UID: "e1", Organizer: "o@x.test"}
	for i := 1; i < maxInvitations; i++ {
		full[i] = invitation{UID: "x" + strconv.Itoa(i), Organizer: "o@x.test"}
	}
	raw, _ := json.Marshal(full)
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("new", "o@x.test"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has("new", "o@x.test"); !ok {
		t.Fatal("newest entry not recorded at the cap")
	}
	if ok, _ := s.Has("e1", "o@x.test"); ok {
		t.Fatal("oldest entry kept past the cap")
	}
	if err := os.WriteFile(s.path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Has("e1", "o@x.test"); err == nil {
		t.Fatal("unreadable record read as no")
	}
}
