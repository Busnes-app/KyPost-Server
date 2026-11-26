//go:build kye2e

package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// In the end-to-end test build multi mode has no bootstrap administrator to make (K6), so
// bootstrap is a no-op that writes nothing, and the backup commands still refuse multi.
func TestE2EBuildBootstrapsNothingAndKeepsBackupsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONFIG_DIR", dir)
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	if err := BootstrapAdmin(); err != nil {
		t.Fatalf("BootstrapAdmin in multi mode: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("multi bootstrap wrote %v", entries)
	}
	for _, name := range []string{"backup-drill", "deposit"} {
		if err := runBackupCommand(name, nil, strings.NewReader(""), new(strings.Builder)); !errors.Is(err, config.ErrMultiTenancyUnavailable) {
			t.Fatalf("%s in multi mode: %v", name, err)
		}
	}
}
