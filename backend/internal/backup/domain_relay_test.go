package backup

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Relay credentials, direct DKIM keys and a direct profile sealed before DKIM
// signing existed (no "dkim" field) all survive a sealed restore unchanged.
func TestDomainRelaySurvivesSealedRestoreAndRejectsIncompleteSnapshots(t *testing.T) {
	relayProfile := mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "operator-login", Password: "operator-secret"}
	directProfile := mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"}
	for _, c := range []struct {
		name    string
		profile mailmsg.DomainRelay
		keyless bool
	}{{"relay", relayProfile, false}, {"direct", directProfile, false}, {"direct-before-dkim", directProfile, true}} {
		t.Run(c.name, func(t *testing.T) { domainRelaySealedRestore(t, c.profile, c.keyless) })
	}
}

func domainRelaySealedRestore(t *testing.T, profile mailmsg.DomainRelay, keyless bool) {
	s := validService(t)
	domains := sso.NewNativeDomainStore(s.dirs.Config)
	if _, err := domains.Configure(context.Background(), "example.test", "https://idp.example"); err != nil {
		t.Fatal(err)
	}
	path, keyPath := filepath.Join(s.dirs.Config, "native-relay.json"), filepath.Join(s.dirs.Secret, "native-relay.key")
	relay, err := mailmsg.SaveDomainRelay(context.Background(), path, keyPath, profile)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Mode == "direct" && len(relay.DKIM) != 1 {
		t.Fatal("direct profile saved without a key")
	}
	if keyless {
		relay.DKIM = nil
		master, err := cryptutil.LoadKey(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := mailmsg.SealDomainRelay(relay, master)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := pinTestKey(t, s)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	manifest, _, err := capsule.Open(raw, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, exists, err := mailmsg.ReadDomainRelay(filepath.Join(dir, "config/native-relay.json"), filepath.Join(dir, "private/native-relay.key"))
	if err != nil || !exists || !restored.Equal(relay) {
		t.Fatal("restored relay/key mismatch", exists, err)
	}
	for _, check := range drillChecks(dir, manifest) {
		if !check.Passed {
			t.Error("drill", check.Name)
		}
	}
	native, err := QuarantineNativeRestore(dir)
	if err != nil || !native || !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
		t.Fatal("relay-only restore was not held", native, err)
	}
	// The matching key is mandatory; do not seal something we cannot decrypt.
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(); err == nil {
		t.Fatal("collector sealed relay without key")
	}
	if err := os.WriteFile(keyPath, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	domainBytes, err := os.ReadFile(filepath.Join(s.dirs.Config, sso.NativeDomainsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dirs.Config, sso.NativeDomainsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(); err == nil {
		t.Fatal("collector sealed relay without domain authority")
	}
	if err := os.WriteFile(filepath.Join(s.dirs.Config, sso.NativeDomainsFile), domainBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private/native-relay.key"), []byte("corrupt-key"), 0600); err != nil {
		t.Fatal(err)
	}
	passed := true
	for _, check := range drillChecks(dir, manifest) {
		passed = passed && check.Passed
	}
	if passed {
		t.Fatal("drill accepted an unusable restored relay")
	}
}

type rotationTXT map[string]string

func (d rotationTXT) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := d[name]; ok {
		return []string{v}, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (rotationTXT) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return nil, &net.DNSError{IsNotFound: true}
}
func (rotationTXT) LookupAddr(context.Context, string) ([]string, error) {
	return nil, &net.DNSError{IsNotFound: true}
}

// A capsule taken mid-rotation (active, next and retiring keys) restores byte
// for byte; afterwards only an administrator's confirm switches, and an
// expired retire window is closed by the first tick.
func TestDomainRelayMidRotationSurvivesSealedRestore(t *testing.T) {
	ctx := context.Background()
	s := validService(t)
	if _, err := sso.NewNativeDomainStore(s.dirs.Config).Configure(ctx, "example.test", "https://idp.example"); err != nil {
		t.Fatal(err)
	}
	dns := rotationTXT{}
	rotation := mailmsg.DKIMRotation{Path: filepath.Join(s.dirs.Config, "native-relay.json"), KeyPath: filepath.Join(s.dirs.Secret, "native-relay.key"), Fence: filepath.Join(s.dirs.Config, sso.NativeDomainsFile), Resolver: dns}
	publish := func(c mailmsg.DomainRelay) {
		for _, r := range c.DKIMRecords(time.Now()) {
			if r.RecordValue != "" {
				dns[r.RecordName+"."] = r.RecordValue
			}
		}
	}
	direct := mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"}
	if _, err := mailmsg.SaveDomainRelay(ctx, rotation.Path, rotation.KeyPath, direct); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-10 * 24 * time.Hour)
	c, _, _, err := rotation.Rotate(ctx, start, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	publish(c)
	if _, _, _, err = rotation.Confirm(ctx, start, "example.test"); err != nil {
		t.Fatal(err)
	}
	// A normal rotation waits for the retire window, so take a next key from
	// another profile to seal all three states at once.
	other := mailmsg.DKIMRotation{Path: filepath.Join(t.TempDir(), "relay"), KeyPath: filepath.Join(t.TempDir(), "key"), Fence: filepath.Join(t.TempDir(), "fence")}
	if _, err := mailmsg.SaveDomainRelay(ctx, other.Path, other.KeyPath, direct); err != nil {
		t.Fatal(err)
	}
	_, next, _, err := other.Rotate(ctx, time.Now(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err = mailmsg.ReadDomainRelay(rotation.Path, rotation.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	c.DKIM = []mailmsg.DKIMKey{c.DKIM[0], next, c.DKIM[1]} // active, next, retiring
	master, err := cryptutil.LoadKey(rotation.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mailmsg.SealDomainRelay(c, master)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotation.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	publish(c)
	key := pinTestKey(t, s)
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capsuleRaw, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	manifest, _, err := capsule.Open(capsuleRaw, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	restored := mailmsg.DKIMRotation{Path: filepath.Join(dir, "config/native-relay.json"), KeyPath: filepath.Join(dir, "private/native-relay.key"), Fence: filepath.Join(dir, "config", sso.NativeDomainsFile), Resolver: dns}
	got, _, err := mailmsg.ReadDomainRelay(restored.Path, restored.KeyPath)
	if err != nil || !got.Equal(c) {
		t.Fatal("mid-rotation keys not restored byte for byte", err)
	}
	for _, check := range drillChecks(dir, manifest) {
		if !check.Passed {
			t.Error("drill", check.Name)
		}
	}
	retiring := c.DKIM[2]
	if _, events, err := restored.Tick(ctx, retiring.KeepPublishedUntil); err != nil || len(events) != 1 || events[0].Phase != "destroy" {
		t.Fatal("expired retire window not closed by the first tick", events, err)
	}
	after, _, _ := mailmsg.ReadDomainRelay(restored.Path, restored.KeyPath)
	if after.DKIM[0].Selector != c.DKIM[0].Selector {
		t.Fatal("tick switched keys")
	}
	if after, _, _, err = restored.Confirm(ctx, time.Now(), "example.test"); err != nil || after.DKIM[0].Selector != next.Selector {
		t.Fatal("confirm after restore", err)
	}
}
