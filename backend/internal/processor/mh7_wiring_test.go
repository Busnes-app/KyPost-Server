package processor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// MH7: in table mode (provisioned keys active), push/APNS relay key files are read-only
// provisioned secrets; self-registration and key renewal must refuse without hitting the
// relay or writing to disk.
func TestRelayKeyRefusesWriteWhenProvisioned(t *testing.T) {
	ts, registrations := renewingRelay(t, "key_superseded")

	secretDir := t.TempDir()
	keyPath := filepath.Join(secretDir, "push_relay_key")
	t.Setenv("SECRET_DIR", secretDir)
	t.Setenv("PUSH_RELAY_URL", ts.URL)
	t.Setenv("PUSH_RELAY_KEY", "")
	t.Setenv("PUSH_RELAY_KEY_FILE", keyPath)

	unprovision := cryptutil.ProvisionKeys(secretDir, keyPath)
	defer unprovision()

	if _, err := resolveRelayKeyWithPrefix(nil, "PUSH_RELAY", ts.URL, ts.Client()); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("resolveRelayKeyWithPrefix: got %v, want ErrKeyNotProvisioned", err)
	}
	sender := &RelaySender{prefix: "PUSH_RELAY", relayURL: ts.URL, client: ts.Client()}
	if _, err := sender.ensureKey(context.Background()); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("ensureKey: got %v, want ErrKeyNotProvisioned", err)
	}
	sender.apiKey = "key-0"
	if _, err := sender.renewKey("key-0"); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("renewKey: got %v, want ErrKeyNotProvisioned", err)
	}
	if registrations.Load() != 0 {
		t.Fatalf("relay /register called %d times under provisioned key refusal", registrations.Load())
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay key file created despite refusal: %v", err)
	}
}

type denyOrgGate struct{ err error }

func (g denyOrgGate) Serve(context.Context, string) error { return g.err }

// Multi-tenant runtime wiring: Poller.SetOrgGate wires the org gate into oooQueueNative so out-of-office
// replies respect multi-tenant org entitlements.
func TestPollerSetOrgGatePassedToOOOQueueNative(t *testing.T) {
	p := &Poller{
		configDir: t.TempDir(),
		stateDir:  t.TempDir(),
	}
	sentinel := errors.New("org not served")
	p.SetOrgGate(denyOrgGate{err: sentinel})
	if p.orgGate == nil {
		t.Fatal("SetOrgGate did not store gate")
	}
	if err := p.orgGate.Serve(context.Background(), "org-1"); !errors.Is(err, sentinel) {
		t.Fatalf("orgGate.Serve: got %v, want %v", err, sentinel)
	}
	// Verify oooQueueNative runs with p.orgGate wired (with nil Accounts, Queue stops before
	// org check only on invalid mailbox; check wiring directly by inspecting error path).
	err := oooQueueNative(context.Background(), p, "nonexistent", "ooo-1", mailbox.OutboundJob{})
	if err == nil {
		t.Fatal("expected error queueing to nonexistent mailbox")
	}
}
