package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestDomainRelaySurvivesSealedRestoreAndRejectsIncompleteSnapshots(t *testing.T) {
	s := validService(t)
	domains := sso.NewNativeDomainStore(s.dirs.Config)
	if _, err := domains.Configure(context.Background(), "example.test", "https://idp.example"); err != nil {
		t.Fatal(err)
	}
	path, keyPath := filepath.Join(s.dirs.Config, "native-relay.json"), filepath.Join(s.dirs.Secret, "native-relay.key")
	relay, err := mailmsg.SaveDomainRelay(context.Background(), path, keyPath, mailmsg.DomainRelay{
		Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "smtp.example.test", Port: 465, Username: "operator-login", Password: "operator-secret",
	})
	if err != nil {
		t.Fatal(err)
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
