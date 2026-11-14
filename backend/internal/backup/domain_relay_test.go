package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
