package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The api and daemon processes run the same Run, so a bad value stops both
// before either opens a store or binds a port.
func TestRunRefusesBadConfigInEveryMode(t *testing.T) {
	for _, mode := range []string{"server", "daemon", "all"} {
		t.Run(mode, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("CONFIG_DIR", configDir)
			t.Setenv("STATE_DIR", t.TempDir())
			t.Setenv("SECRET_DIR", t.TempDir())

			t.Setenv("WEB_PORT", "58b6")
			if err := Run([]string{"--mode", mode}); err == nil || !strings.Contains(err.Error(), "WEB_PORT") {
				t.Fatalf("malformed WEB_PORT: %v", err)
			}
			t.Setenv("WEB_PORT", "")

			if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("redacton:\n  patterns: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Run([]string{"--mode", mode}); err == nil || !strings.Contains(err.Error(), "redacton") {
				t.Fatalf("unknown config.yaml key: %v", err)
			}
		})
	}
}

// KYPOST_TENANCY=multi refuses every mode and bootstrap-admin before anything is written,
// until hosted mail lands (tenancy design, build plan PR 7).
func TestRunRefusesMultiTenancy(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CONFIG_DIR", configDir)
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("SECRET_DIR", t.TempDir())
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	for _, mode := range []string{"server", "daemon", "all", "bootstrap-admin"} {
		if err := Run([]string{"--mode", mode}); err == nil || !strings.Contains(err.Error(), "KYPOST_TENANCY=multi is not available") {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	for _, f := range []string{"users.json", "admin.env", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(configDir, f)); !os.IsNotExist(err) {
			t.Errorf("%s was written: %v", f, err)
		}
	}
}
