package kydrive

import (
	"bytes"
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
	if err := os.WriteFile(short, []byte(strings.Repeat("s", MinSecretBytes-1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		url, file, private string
		on, fails          bool
	}{
		"both unset":        {},
		"configured":        {url: "https://drive.example.com", file: good, on: true},
		"trailing slash":    {url: "https://drive.example.com/", file: good, on: true},
		"url only":          {url: "https://drive.example.com", fails: true},
		"file only":         {file: good, fails: true},
		"http":              {url: "http://drive.example.com", file: good, fails: true},
		"path":              {url: "https://drive.example.com/x", file: good, fails: true},
		"credentials":       {url: "https://u:p@drive.example.com", file: good, fails: true},
		"loopback literal":  {url: "https://127.0.0.1", file: good, fails: true},
		"localhost":         {url: "https://localhost:8443", file: good, fails: true},
		"short secret":      {url: "https://drive.example.com", file: short, fails: true},
		"missing secret":    {url: "https://drive.example.com", file: filepath.Join(dir, "nope"), fails: true},
		"bad allow-private": {url: "https://drive.example.com", file: good, private: "yes", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KYPOST_KYDRIVE_URL", tc.url)
			t.Setenv("KYPOST_KYDRIVE_SECRET_FILE", tc.file)
			t.Setenv("KYPOST_KYDRIVE_ALLOW_PRIVATE", tc.private)
			c, err := FromEnv()
			if (err != nil) != tc.fails || c.Available() != tc.on {
				t.Fatalf("FromEnv() = %v, %v; want on=%v fails=%v", c, err, tc.on, tc.fails)
			}
			if err != nil && strings.Contains(err.Error(), string(testSecret)) {
				t.Fatal("error carries the secret")
			}
			if tc.on && (string(c.link.Load().secret) != string(testSecret) || c.Origin() != "https://drive.example.com") {
				t.Fatalf("secret not trimmed or base not normalised: %s", c.Origin())
			}
		})
	}
}

func TestOnOrigin(t *testing.T) {
	base, _ := parseBase("https://drive.example.com")
	c := New(base, testSecret, nil)
	for raw, want := range map[string]bool{
		"https://drive.example.com/api/suite/upload/abc":       true,
		"https://drive.example.com/api/suite/upload/":          false,
		"https://drive.example.com/api/other/abc":              false,
		"https://drive.example.com/api/suite/upload/../x":      false,
		"http://drive.example.com/api/suite/upload/abc":        false,
		"https://evil.example.com/api/suite/upload/abc":        false,
		"https://drive.example.com:444/api/suite/upload/abc":   false,
		"https://u@drive.example.com/api/suite/upload/abc":     false,
		"https://drive.example.com.evil/api/suite/upload/abc":  false,
		"//drive.example.com/api/suite/upload/abc":             false,
		"javascript:alert(1)//drive.example.com/api/suite/up/": false,
	} {
		if got := c.OnOrigin(raw, UploadPrefix); got != want {
			t.Errorf("OnOrigin(%q) = %v", raw, got)
		}
	}
	if !c.OnOrigin("https://drive.example.com/l/#tok", LinkPrefix) || c.OnOrigin("https://drive.example.com/l/", LinkPrefix) {
		t.Error("link token is the fragment")
	}
	var unpaired *Client
	if unpaired.OnOrigin("https://drive.example.com/api/suite/upload/abc", UploadPrefix) || unpaired.Origin() != "" || unpaired.Available() {
		t.Fatal("nil client reads as paired")
	}
}

// The guarded transport refuses a loopback server even with private allowed.
func TestGuardedTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, testSecret, guardedTransport(true))
	if _, _, err := c.Post(t.Context(), PathStatus, EventStatus, "id-1", map[string]string{}); !errors.Is(err, netguard.ErrInternalAddress) {
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
		ev, err := syncauth.Verify(testSecret, syncauth.FromRequest(r), body, syncauth.Options{})
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Bad signature","code":"bad_signature"}`))
			return
		}
		got = append(got, r.URL.Path+" "+ev.Type+" "+ev.ID+" "+string(body))
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, testSecret, srv.Client().Transport)
	if status, _, err := c.Post(t.Context(), PathStatus, EventStatus, "ev-1", map[string]string{"sub": "s"}); err != nil || status != 200 {
		t.Fatalf("Post = %d %v", status, err)
	}
	if want := PathStatus + " " + EventStatus + ` ev-1 {"sub":"s"}`; len(got) != 1 || got[0] != want {
		t.Fatalf("KyDrive saw %q", got)
	}
	wrong := New(base, []byte(strings.Repeat("x", MinSecretBytes)), srv.Client().Transport)
	if status, out, _ := wrong.Post(t.Context(), PathStatus, EventStatus, "ev-2", map[string]string{}); status != 401 || Outcome(out) != "bad_signature" {
		t.Fatalf("wrong secret answered %d %s", status, out)
	}
	if status, _, err := c.Post(t.Context(), "/redirect", EventStatus, "ev-3", map[string]string{}); err != nil || status != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("redirect followed: %d %v %v", status, err, redirected)
	}
}

// Upload sends one PUT when the file fits a chunk and ordered Content-Range
// chunks otherwise, stops at the first refusal, and never sends a ticket off
// the paired origin.
func TestUpload(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var assembled bytes.Buffer
	refuseAt := "none"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Range"))
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Ky-Signature") != "" || int64(len(body)) != r.ContentLength {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Content-Range") == refuseAt {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"x","code":"bad_range"}`))
			return
		}
		assembled.Write(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"file_id":"f1","name":"a.bin","size":10}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := New(base, testSecret, srv.Client().Transport)
	ticket := srv.URL + UploadPrefix + "tok"
	data := []byte("0123456789")

	status, out, err := c.Upload(t.Context(), ticket, bytes.NewReader(data), 10, 64)
	if err != nil || status != 201 || len(seen) != 1 || seen[0] != "PUT /api/suite/upload/tok " || assembled.String() != string(data) {
		t.Fatalf("single upload: %d %s %v %q", status, out, err, seen)
	}
	seen, assembled = nil, bytes.Buffer{}
	status, _, err = c.Upload(t.Context(), ticket, bytes.NewReader(data), 10, 4)
	if err != nil || status != 201 || assembled.String() != string(data) ||
		strings.Join(seen, ",") != "PUT /api/suite/upload/tok bytes 0-3/10,PUT /api/suite/upload/tok bytes 4-7/10,PUT /api/suite/upload/tok bytes 8-9/10" {
		t.Fatalf("chunked upload: %d %v %q", status, err, seen)
	}
	seen, refuseAt = nil, "bytes 4-7/10"
	if status, out, _ := c.Upload(t.Context(), ticket, bytes.NewReader(data), 10, 4); status != 409 || Outcome(out) != "bad_range" || len(seen) != 2 {
		t.Fatalf("refused chunk did not stop the upload: %d %q", status, seen)
	}
	seen = nil
	if _, _, err := c.Upload(t.Context(), "https://evil.example.com"+UploadPrefix+"tok", bytes.NewReader(data), 10, 4); err == nil || len(seen) != 0 {
		t.Fatal("ticket off the paired origin was used")
	}
	srv.Close()
	if _, _, err := c.Upload(t.Context(), ticket, bytes.NewReader(data), 10, 4); err == nil || strings.Contains(err.Error(), "tok") {
		t.Fatalf("network error carries the ticket: %v", err)
	}
}

// fakeIdentity answers /api/suite/peer?client=kydrive for KyPost's credentials.
type fakeIdentity struct {
	mu     sync.Mutex
	status int
	url    string
	srv    *httptest.Server
}

func newFakeIdentity(t *testing.T) *fakeIdentity {
	f := &fakeIdentity{status: 200, url: "https://example.com"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id, pw, ok := r.BasicAuth()
		switch {
		case r.URL.Path != "/api/suite/peer" || r.URL.Query().Get("client") != "kydrive":
			w.WriteHeader(http.StatusBadRequest)
		case !ok || id != "kypost" || pw != "client-secret":
			w.WriteHeader(http.StatusUnauthorized)
		case f.status == http.StatusNotFound:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no_peer"}`))
		case f.status != http.StatusOK:
			w.WriteHeader(f.status)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"client": "kydrive", "url": f.url, "secret": base64.StdEncoding.EncodeToString(testSecret)})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		ip     string
		noSSO  bool
		on     bool
		reason string
	}{
		"paired":           {status: 200, ip: "8.8.8.8", on: true, reason: "Connected"},
		"private, refused": {status: 200, ip: "192.168.1.20", reason: "KYPOST_KYDRIVE_ALLOW_PRIVATE"},
		"loopback":         {status: 200, ip: "127.0.0.1", reason: "never allowed"},
		"no peer":          {status: 404, ip: "8.8.8.8", reason: "no KyDrive"},
		"identity down":    {status: 503, ip: "8.8.8.8", reason: "could not be reached"},
		"sso not set up":   {noSSO: true, reason: "does not sign in"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KYPOST_KYDRIVE_URL", "")
			t.Setenv("KYPOST_KYDRIVE_SECRET_FILE", "")
			t.Setenv("KYPOST_KYDRIVE_ALLOW_PRIVATE", "")
			id := newFakeIdentity(t)
			id.status = tc.status
			c, err := FromEnv()
			if err != nil || c.Available() || c.dir == nil {
				t.Fatalf("FromEnv = %v %v", c, err)
			}
			c.dir.http = id.srv.Client()
			c.dir.lookup = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP(tc.ip)}, nil }
			d := Directory{Issuer: id.srv.URL, ClientID: "kypost", ClientSecret: "client-secret"}
			if tc.noSSO {
				d = Directory{}
			}
			err = c.resolve(t.Context(), d)
			st := c.Status()
			if c.Available() != tc.on || st.Connected != tc.on || (err == nil) != (tc.on || tc.noSSO) || !strings.Contains(st.Reason, tc.reason) {
				t.Fatalf("resolve = %v, status %+v", err, st)
			}
			if tc.on && (c.Origin() != "https://example.com" || st.Source != SourceKyIdentity) {
				t.Fatalf("paired link wrong: %+v", st)
			}
			raw, _ := json.Marshal(st)
			if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(testSecret)) || strings.Contains(string(raw), "client-secret") {
				t.Fatal("status carries a secret")
			}
		})
	}
}
