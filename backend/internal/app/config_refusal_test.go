package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
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
	t.Setenv("KY_CELLS", "test-1")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	setObjectStorageEnv(t)
	for _, mode := range []string{"server", "daemon", "all", "bootstrap-admin"} {
		err := Run([]string{"--mode", mode})
		refused := err != nil && strings.Contains(err.Error(), "KYPOST_TENANCY=multi is not available")
		// The end-to-end test build passes the gate: bootstrap has nothing to do, and every
		// other mode then refuses the cell that has no masked relay region code (MH9).
		noCode := errors.Is(err, tenancy.ErrNoRelayCode)
		if taggedBuild && (refused || mode != "bootstrap-admin" && !noCode) || !taggedBuild && !refused {
			t.Fatalf("%s, test build %v: %v", mode, taggedBuild, err)
		}
	}
	for _, f := range []string{"users.json", "admin.env", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(configDir, f)); !os.IsNotExist(err) {
			t.Errorf("%s was written: %v", f, err)
		}
	}
}

// setObjectStorageEnv sets well-formed KYPOST_S3_* that no test ever reaches.
func setObjectStorageEnv(t *testing.T) {
	t.Setenv("KYPOST_S3_ENDPOINT", "https://s3.example.test")
	t.Setenv("KYPOST_S3_BUCKET", "kypost-test")
	t.Setenv("KYPOST_S3_ACCESS_KEY", "test-access")
	t.Setenv("KYPOST_S3_SECRET_KEY", "test-secret-value")
}

// A multi start without object storage is refused before anything is written (MH11), and
// the refusal never repeats a credential.
func TestRunRefusesMultiWithoutObjectStorage(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CONFIG_DIR", configDir)
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("SECRET_DIR", t.TempDir())
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KY_CELLS", "test-1")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	t.Setenv("KYPOST_S3_ACCESS_KEY", "test-access")
	t.Setenv("KYPOST_S3_SECRET_KEY", "test-secret-value")
	for _, mode := range []string{"server", "daemon", "all"} {
		err := Run([]string{"--mode", mode})
		if err == nil || taggedBuild && !strings.Contains(err.Error(), "requires object storage") || strings.Contains(err.Error(), "test-secret-value") {
			t.Fatalf("%s, test build %v: %v", mode, taggedBuild, err)
		}
	}
	if _, err := os.Stat(filepath.Join(configDir, "users.json")); !os.IsNotExist(err) {
		t.Errorf("users.json was written: %v", err)
	}
}
