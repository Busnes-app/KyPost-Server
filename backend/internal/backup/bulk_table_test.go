package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
)

// MH7: in table mode (provisioned keys active), bulk-backup.repo is a TargetKey and must
// be provisioned by the operator rather than created on host disk by bulkKey.
func TestBulkKeyRefusesCreateWhenProvisioned(t *testing.T) {
	secretDir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cryptutil.LoadOrCreateKey(filepath.Join(secretDir, bulkKeyName)); err != nil {
		t.Fatal(err)
	}

	fakeRestic := filepath.Join(t.TempDir(), "restic")
	script := "#!/bin/sh\necho '{\"id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"}'\n"
	if err := os.WriteFile(fakeRestic, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	unprovision := cryptutil.ProvisionKeys(secretDir)
	defer unprovision()

	s := &Service{
		dirs: Dirs{Secret: secretDir},
		cfg:  config.BackupConfig{BulkRepository: repoDir, ResticBinary: fakeRestic},
	}
	if _, err := s.bulkKey(context.Background()); !errors.Is(err, cryptutil.ErrKeyNotProvisioned) {
		t.Fatalf("bulkKey without provisioned bulk-backup.repo: got %v, want ErrKeyNotProvisioned", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, bulkRepoIDName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused bulkKey wrote bulk-backup.repo: %v", err)
	}
}
