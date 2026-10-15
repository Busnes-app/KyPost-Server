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
