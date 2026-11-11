//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
)

// A v3 snapshot (native-authority.db beside both stubs) is collected as a
// SQLite snapshot, seals, opens, drills and validates. A half-migrated mix
// refuses collection. No binary writes v3 yet, so the fixture is converted.
func TestNativeBackupAcceptsV3Snapshot(t *testing.T) {
	s, _ := nativeService(t)
	v2Ledger, err := os.ReadFile(ssotest.LedgerPath(s.dirs.Config))
	if err != nil {
		t.Fatal(err)
	}
	if err := sso.WriteNativeAuthorityV3ForTest(s.dirs.Config); err != nil {
		t.Fatal(err)
	}
	// Migration copies are never collected.
	if err := os.WriteFile(ssotest.LedgerPath(s.dirs.Config)+".v2-migrated", v2Ledger, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := s.Collect()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, f := range payload.Files {
		paths[f.Path] = true
	}
	if !paths["config/"+sso.NativeAuthorityFile] || !paths["config/native-provisioning.json"] || paths["config/native-provisioning.json.v2-migrated"] || paths["config/"+sso.NativeAuthorityFile+"-wal"] {
		t.Fatal("v3 payload", paths)
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
	integrity := false
	for _, check := range drillChecks(dir, manifest) {
		if !check.Passed {
			t.Error("v3 drill", check.Name)
		}
		integrity = integrity || check.Name == "sqlite:config/"+sso.NativeAuthorityFile
	}
	if !integrity {
		t.Error("drill did not check the v3 database's integrity")
	}
	if native, err := nativeSnapshot(dir); err != nil || !native {
		t.Fatal("restored v3 snapshot", native, err)
	}
	// The restore fences write authority; until the SQLite backend lands they
	// cannot write v3, so the restore stays held and unpublished.
	native, err := QuarantineNativeRestore(dir)
	if err == nil || !native || !strings.Contains(err.Error(), "cannot fence restored native") {
		t.Fatal("v3 restore qualified without a v3 writer", native, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "state", sso.NativeRestoreHoldFile)); err != nil {
		t.Fatal("v3 restore not held", err)
	}

	// A v2 ledger beside the database is half-migrated.
	if err := os.WriteFile(ssotest.LedgerPath(s.dirs.Config), v2Ledger, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(); err == nil {
		t.Fatal("v2 ledger beside native-authority.db collected")
	}
}
