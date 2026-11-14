//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// writeKeylessRelay stores c exactly as a binary before DKIM signing did.
func writeKeylessRelay(t *testing.T, c mailmsg.DomainRelay) (path, keyPath string) {
	t.Helper()
	path, keyPath = filepath.Join(config.ConfigDir(), "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	key, err := cryptutil.LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c.DKIM = nil
	raw, err := mailmsg.SealDomainRelay(c, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, keyPath
}

func TestMigrateNativeAddsDKIMKeysToDirectProfile(t *testing.T) {
	t.Setenv("CONFIG_DIR", t.TempDir())
	t.Setenv("SECRET_DIR", t.TempDir())
	direct := mailmsg.DomainRelay{Mode: "direct", Generation: "12345678-1234-4234-8234-123456789abc", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"}
	path, keyPath := writeKeylessRelay(t, direct)
	// The native domain fence is held: reconciliation waits rather than write.
	release, err := fsutil.LockFileContext(context.Background(), filepath.Join(config.ConfigDir(), sso.NativeDomainsFile))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = reconcileDKIMKeys(ctx, config.ConfigDir(), keyPath)
	cancel()
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("reconciliation ignored the native domain lock", err)
	}
	if err := Run([]string{"migrate-native"}); err != nil {
		t.Fatal(err)
	}
	keyed, _, err := mailmsg.ReadDomainRelay(path, keyPath)
	if err != nil || keyed.Generation != direct.Generation || len(keyed.DKIM) != 1 || keyed.DKIM[0].Domain != "example.test" {
		t.Fatal("upgrade did not add a key under the same generation", err, keyed.DKIM)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"migrate-native"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("second start rewrote the profile")
	}
	relay := mailmsg.DomainRelay{Generation: direct.Generation, Domains: direct.Domains, Issuer: direct.Issuer, Host: "smtp.example.test", Port: 465, Username: "u", Password: "p"}
	writeKeylessRelay(t, relay)
	if err := Run([]string{"migrate-native"}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := mailmsg.ReadDomainRelay(path, keyPath); err != nil || len(got.DKIM) != 0 {
		t.Fatal("relay profile got keys", err)
	}
}

// setup-status reads the relay but reports only booleans.
func TestSetupStatusHidesDKIMKey(t *testing.T) {
	setupTestRoots(t)
	b := setupBundle{Version: 1, SSO: &sso.SSOSettings{Enabled: true, IssuerURL: "https://identity.example.com", ClientID: "kypost", ClientSecret: "private-test-client-secret"}, PairingSecret: setupTestKey(t), Domains: []string{"example.com"}}
	if _, e := setupTestBundle(t, b); e != nil {
		t.Fatal(e)
	}
	relay, err := mailmsg.SaveDomainRelay(context.Background(), filepath.Join(config.ConfigDir(), "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.com"}, Issuer: "https://identity.example.com", Host: "mail.example.com"})
	if err != nil || len(relay.DKIM) != 1 {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if e := runSetupStatus(nil, &out); e != nil {
		t.Fatal(e)
	}
	encoded := base64.StdEncoding.EncodeToString(relay.DKIM[0].Private)
	for i := 0; i+32 <= len(encoded); i += 16 {
		if bytes.Contains(out.Bytes(), []byte(encoded[i:i+32])) {
			t.Fatal("setup-status leaked the DKIM key")
		}
	}
	if bytes.Contains(out.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("setup-status leaked a private key")
	}
}
