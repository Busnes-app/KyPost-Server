//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceivingRspamdProtocol(t *testing.T) {
	raw := []byte("Subject: unchanged\r\nX-Spam-Flag: No\r\n\r\nbody\r\n")
	d := ingress.Delivery{ID: "trusted-id", Sender: "sender@example.test", Bindings: []ingress.Binding{{Address: "one@example.test"}, {Address: "two@example.test"}}}
	for _, tc := range []struct {
		name, body   string
		status, code int
		ok           bool
	}{
		{"clean", `{"action":"no action","is_skipped":false}`, 200, 0, true},
		{"tag", `{"action":"add header","is_skipped":false}`, 200, 0, true},
		{"subject", `{"action":"rewrite subject","is_skipped":false}`, 200, 0, true},
		{"GTUBE", `{"action":"reject","is_skipped":true}`, 200, 4, false},
		{"reject", `{"action":"reject"}`, 200, 4, false},
		{"soft", `{"action":"soft reject"}`, 200, 0, false},
		{"grey", `{"action":"greylist"}`, 200, 0, false},
		{"unknown", `{"action":"attacker"}`, 200, 0, false},
		{"missing skipped", `{"action":"no action"}`, 200, 0, false},
		{"null skipped", `{"action":"no action","is_skipped":null}`, 200, 0, false},
		{"empty", `{}`, 200, 0, false},
		{"skipped", `{"action":"no action","is_skipped":true}`, 200, 0, false},
		{"error", `{"action":"no action","error":"sensitive-provider-content"}`, 200, 0, false},
		{"malformed", `{"action":`, 200, 0, false},
		{"trailing", `{"action":"no action"} {}`, 200, 0, false},
		{"large", strings.Repeat("x", (256<<10)+1), 200, 0, false},
		{"failure", `sensitive-provider-content`, 500, 0, false},
		{"redirect", ``, 302, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.Header.Get("IP") != "192.0.2.2" || r.Header.Get("From") != d.Sender || len(r.Header.Values("Rcpt")) != 2 || r.Header.Get("Pass") != "all" {
					t.Error("wrong trusted envelope")
				}
				var payload bytes.Buffer
				_, _ = payload.ReadFrom(r.Body)
				if !bytes.Equal(payload.Bytes(), raw) {
					t.Error("payload changed")
				}
				w.Header().Set("Location", "http://invalid.example.test/")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := scanReceivingSpam(context.Background(), raw, d, "192.0.2.2", "claimed.example.test", server.URL)
			if (err == nil) != tc.ok {
				t.Fatal("unexpected result", err)
			}
			var commandError *receivingCommandError
			if tc.code != 0 && (!errors.As(err, &commandError) || commandError.code != tc.code) {
				t.Fatal("wrong refusal", err)
			}
			if err != nil && strings.Contains(err.Error(), "sensitive-provider-content") {
				t.Fatal("response leak")
			}
		})
	}
	for _, peer := range []struct{ ip, helo string }{{"bad", "a"}, {"192.0.2.2", "a\r\nInjected: yes"}, {"fe80::1%eth0", "a"}} {
		if scanReceivingSpam(context.Background(), raw, d, peer.ip, peer.helo, "http://127.0.0.1:1") == nil {
			t.Fatal("unsafe peer admitted")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer slow.Close()
	if scanReceivingSpam(ctx, raw, d, "192.0.2.2", "a", slow.URL) == nil {
		t.Fatal("deadline bypass")
	}
	for _, value := range []string{"TRUE", "1", "false\n"} {
		t.Setenv("KYPOST_RECEIVING_RSPAMD", value)
		if _, err := receivingRspamdEnabled(); err == nil {
			t.Fatal("invalid opt-in")
		}
	}
}

func TestNativeReceivingRspamdRetentionAndAuthority(t *testing.T) {
	lockReceivingRspamdProof(t)
	r, created := receivingFixture(t)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
	listener, err := net.Listen("tcp", "127.0.0.1:11333")
	if err != nil {
		t.Fatal("test scanner port occupied", err)
	}
	revoke := false
	scanner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if revoke {
			if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
				t.Error(err)
			}
		}
		_, _ = w.Write([]byte(`{"action":"no action","is_skipped":false}`))
	}))
	scanner.Listener = listener
	scanner.Start()
	defer scanner.Close()
	raw := []byte("From: sender@example.test\r\nSubject: exact bytes\r\n\r\nbody\r\n")
	ctx := context.Background()
	if err := r.bind(ctx, "accepted", "sender@example.test", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "accepted", "sender@example.test", bytes.NewReader(raw), "192.0.2.2", "claimed.example.test"); err != nil {
		t.Fatal(err)
	}
	revoke = true
	if err := r.bind(ctx, "revoked", "sender@example.test", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "revoked", "sender@example.test", bytes.NewReader(raw), "192.0.2.2", "claimed.example.test"); err == nil {
		t.Fatal("revocation during scan bypassed authority")
	}
	staged, err := r.holding.Get(ctx, receivingGateway, "revoked")
	if err != nil || staged.State != "staged" || len(staged.Raw) != 0 {
		t.Fatal("revoked payload published", err)
	}
	// Use the remaining active owner to prove accepted replay during outage.
	revoke = false
	if err := r.bind(ctx, "replay", "sender@example.test", "two@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "replay", "sender@example.test", bytes.NewReader(raw), "192.0.2.2", "a"); err != nil {
		t.Fatal(err)
	}
	scanner.Close()
	if err := r.accept(ctx, "replay", "sender@example.test", bytes.NewReader(raw)); err != nil {
		t.Fatal("accepted replay depends on scanner", err)
	}
	if err := r.accept(ctx, "replay", "sender@example.test", bytes.NewBufferString("changed")); err == nil {
		t.Fatal("conflicting accepted bytes permitted")
	}
	if err := r.bind(ctx, "outage", "sender@example.test", "two@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, "outage", "sender@example.test", bytes.NewReader(raw), "192.0.2.2", "a"); err == nil {
		t.Fatal("outage silently bypassed scanner")
	}
	got, err := r.holding.Get(ctx, receivingGateway, "replay")
	if err != nil || !bytes.Equal(got.Raw, raw) || got.State != "pending" {
		t.Fatal("accepted mail changed", err)
	}
	got, err = r.holding.Get(ctx, receivingGateway, "outage")
	if err != nil || got.State != "staged" || len(got.Raw) != 0 {
		t.Fatal("outage published mail", err)
	}
}

func lockReceivingRspamdProof(t *testing.T) {
	t.Helper()
	// The production endpoint is fixed. Serialize disposable fixtures across
	// worktrees so concurrent qualification cannot adopt another test's scanner.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "kypost-rspamd-proof")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	release, err := fsutil.LockFileContext(ctx, filepath.Join(dir, "scanner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}
