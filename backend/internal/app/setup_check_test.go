package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestInstallerCheckPreservesEveryArtifact(t *testing.T) {
	setupTestRoots(t)
	secret := filepath.Join(config.SecretDir(), "installer-client-secret")
	if err := os.WriteFile(secret, []byte("test-client-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := setupBundle{Version: 1, SSO: &sso.SSOSettings{Enabled: true, IssuerURL: "https://identity.test", ClientID: "kypost", AutoProvision: true}, ClientSecretFile: secret}
	raw, _ := json.Marshal(b)
	check := func() error {
		return runApplySetup([]string{"--file", "-", "--check"}, bytes.NewReader(raw), &bytes.Buffer{})
	}
	snapshot := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, root := range []string{config.ConfigDir(), config.StateDir(), config.SecretDir()} {
			err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				// Live WAL coordination files may be maintained by SQLite readers;
				// they are not configuration, schema, identity, audit or key writes.
				if strings.HasSuffix(p, "state.db-wal") || strings.HasSuffix(p, "state.db-shm") {
					return nil
				}
				if info.Mode().IsRegular() {
					data, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					result[p] = string(data)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := snapshot()
	if err := check(); err == nil {
		t.Fatal("check accepted absent SSO")
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("missing-state check changed persisted artifacts")
	}
	if _, err := setupTestBundle(t, b); err != nil {
		t.Fatal(err)
	}
	before = snapshot()
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("present-state check changed persisted artifacts")
	}
	var caps bytes.Buffer
	if err := runApplySetup([]string{"--capabilities"}, nil, &caps); err != nil || !bytes.Contains(caps.Bytes(), []byte("kypost.setup/1")) {
		t.Fatal(err, caps.String())
	}
	b.SSO.ClientSecret = "inline-conflict"
	if _, err := setupTestBundle(t, b); err == nil {
		t.Fatal("accepted competing secret sources")
	}
}
