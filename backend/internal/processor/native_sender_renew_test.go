package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// renewingRelay accepts only its newest key and refuses every other one with a 401 carrying
// code: "key_superseded" for a key it replaced, "key_unknown" for one it forgot, "" for a
// revoked key.
func renewingRelay(t *testing.T, code string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var registrations atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/register":
			n := registrations.Add(1)
			time.Sleep(20 * time.Millisecond) // widen the window concurrent senders could stampede in
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"key": fmt.Sprintf("key-%d", n)})
		case "/send":
			if n := registrations.Load(); n > 0 && r.Header.Get("Authorization") == fmt.Sprintf("Bearer key-%d", n) {
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			body := map[string]string{"error": "invalid api key"}
			if code != "" {
				body["code"] = code
			}
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &registrations
}

// renewTestSender is a sender holding the self-registered key "key-0" in a key file.
func renewTestSender(t *testing.T, relayURL string) (*RelaySender, string) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "push_relay_key")
	if err := os.WriteFile(keyFile, []byte("key-0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUSH_RELAY_URL", relayURL)
	t.Setenv("PUSH_RELAY_KEY", "")
	t.Setenv("PUSH_RELAY_KEY_FILE", keyFile)
	sender := newRelaySenderFromEnvWithPrefix(nil, "PUSH_RELAY")
	if sender == nil {
		t.Fatal("no sender")
	}
	return sender, keyFile
}

var renewTestDevice = state.NativeDevice{PushToken: "device-token", Platform: "android"}

func TestRelaySenderReRegistersOnceWhenKeySupersededOrUnknown(t *testing.T) {
	for _, code := range []string{"key_superseded", "key_unknown"} {
		t.Run(code, func(t *testing.T) {
			ts, registrations := renewingRelay(t, code)
			sender, keyFile := renewTestSender(t, ts.URL)

			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- sender.Send(context.Background(), renewTestDevice, NativePushMessage{Title: "t"})
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("send after %s: %v", code, err)
				}
			}
			if n := registrations.Load(); n != 1 {
				t.Fatalf("registrations = %d, want 1: concurrent sends must share one renewal", n)
			}
			if key, _ := os.ReadFile(keyFile); string(key) != "key-1\n" {
				t.Fatalf("key file = %q, want the renewed key", key)
			}
		})
	}
}

func TestRelaySenderKeepsRevokedAndOperatorKeys(t *testing.T) {
	revoked, revokedRegs := renewingRelay(t, "")
	sender, _ := renewTestSender(t, revoked.URL)
	if err := sender.Send(context.Background(), renewTestDevice, NativePushMessage{}); err == nil {
		t.Fatal("send with a revoked key succeeded")
	}

	forgetting, operatorRegs := renewingRelay(t, "key_unknown")
	t.Setenv("PUSH_RELAY_URL", forgetting.URL)
	t.Setenv("PUSH_RELAY_KEY", "key-0")
	operator := newRelaySenderFromEnvWithPrefix(nil, "PUSH_RELAY")
	if err := operator.Send(context.Background(), renewTestDevice, NativePushMessage{}); err == nil {
		t.Fatal("send with a refused operator key succeeded")
	}
	if revokedRegs.Load()+operatorRegs.Load() != 0 {
		t.Fatal("re-registered on a revocation or over an operator-supplied key")
	}
}

// The api and daemon processes share the key file. The one that renews second must pick up
// the sibling's new key rather than register again, which would supersede it.
func TestRelaySenderAdoptsSiblingsRenewedKey(t *testing.T) {
	ts, registrations := renewingRelay(t, "key_superseded")
	sender, keyFile := renewTestSender(t, ts.URL)

	registrations.Store(1)                                                  // the sibling registered key-1 ...
	if err := os.WriteFile(keyFile, []byte("key-1\n"), 0o600); err != nil { // ... and persisted it
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), renewTestDevice, NativePushMessage{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n := registrations.Load(); n != 1 {
		t.Fatalf("registered again over the sibling's key (registrations = %d)", n)
	}
}

func TestRelaySenderReRegistrationIsThrottled(t *testing.T) {
	ts, registrations := renewingRelay(t, "key_unknown")
	sender, _ := renewTestSender(t, ts.URL)
	sender.lastRegAttempt = time.Now() // a registration just happened

	if err := sender.Send(context.Background(), renewTestDevice, NativePushMessage{}); err == nil {
		t.Fatal("send succeeded without a renewal")
	}
	if n := registrations.Load(); n != 0 {
		t.Fatalf("registered inside registerRetryInterval (registrations = %d)", n)
	}
}
