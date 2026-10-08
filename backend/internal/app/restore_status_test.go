package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestRestoreStatusCLI(t *testing.T) {
	stateDir, secretDir := t.TempDir(), t.TempDir()
	t.Setenv("CONFIG_DIR", t.TempDir())
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("SECRET_DIR", secretDir)
	t.Setenv("PAIRING_SECRET", "")
	t.Setenv("PAIRING_SECRET_FILE", "")
	status := func() sso.NativeRestoreReleaseStatus {
		t.Helper()
		var out bytes.Buffer
		if err := runBackupCommand("restore", []string{"status"}, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		var st sso.NativeRestoreReleaseStatus
		if err := json.Unmarshal(out.Bytes(), &st); err != nil {
			t.Fatal(err, out.String())
		}
		return st
	}
	if st := status(); st.Held || len(st.Preconditions) != 0 {
		t.Fatalf("not held: %+v", st)
	}
	if err := fsutil.PersistJSONFile(filepath.Join(stateDir, sso.NativeRestoreHoldFile), map[string]any{"version": 1, "epoch": "12345678-1234-4123-8123-123456789abc"}); err != nil {
		t.Fatal(err)
	}
	st := status()
	if !st.Held || st.Epoch != "12345678-1234-4123-8123-123456789abc" || len(st.Preconditions) != 9 || st.Preconditions[2].ID != "P3" || st.Preconditions[2].OK || !strings.Contains(strings.Join(st.Preconditions[2].Reasons, "\n"), "restore again with this version") {
		t.Fatalf("held: %+v", st)
	}
	// Read-only: the pairing key is read, never generated.
	if entries, _ := os.ReadDir(secretDir); len(entries) != 0 {
		t.Fatalf("status wrote secrets: %v", entries)
	}
}
