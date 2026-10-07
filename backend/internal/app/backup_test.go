package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestRestoreCLIUsesCustodianSharesAndRefusesOverwrite(t *testing.T) {
	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	raw, manifest, err := recoveryclient.Seal(recoveryclient.Payload{ServiceName: backup.AppName, AppVersion: "test", Files: []recoveryclient.File{{Path: "config/test.json", Data: []byte(`{"synthetic":true}`), Mode: 0600}}}, recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.kycap")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restore")
	input := shares[0].String() + "\n" + shares[2].String() + "\n"
	var out bytes.Buffer
	if err := runBackupCommand("restore", []string{path, target}, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "config/test.json"))
	if err != nil || string(got) != `{"synthetic":true}` {
		t.Fatalf("restored payload: %s %v", got, err)
	}
	if err := sso.RequireNativeRestoreReleased(filepath.Join(target, "state")); err != nil {
		t.Fatal("ordinary legacy restore retained a native hold", err)
	}
	if !strings.Contains(out.String(), manifest.CapsuleID) || strings.Contains(out.String(), shares[0].String()) {
		t.Fatal("restore output missing identity or leaked shares")
	}
	if err := runBackupCommand("restore", []string{path, target}, strings.NewReader(input), &out); err == nil {
		t.Fatal("restore overwrote occupied destination")
	}
	if err := runBackupCommand("restore", []string{path, filepath.Join(t.TempDir(), "short")}, strings.NewReader(shares[0].String()), &out); err == nil {
		t.Fatal("one custodian restored a two-share capsule")
	}
}

func TestRestoreCLIFailurePreservesNativeDataWithoutSuccess(t *testing.T) {
	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	users := []byte(`{"users":[{"id":"one","ssoSub":"one","nativeMailboxIssuer":"https://identity.example","nativeMailboxSource":"native:claimed"}]}`)
	raw, _, err := recoveryclient.Seal(recoveryclient.Payload{ServiceName: backup.AppName, AppVersion: "test", Files: []recoveryclient.File{{Path: "config/users.json", Data: users, Mode: 0600}}}, recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3})
	if err != nil {
		t.Fatal(err)
	}
	path, target := filepath.Join(t.TempDir(), "partial.kycap"), filepath.Join(t.TempDir(), "restore")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runRestore([]string{path, target}, strings.NewReader(shares[0].String()+"\n"+shares[1].String()+"\n"), &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("partial native restore reported success: output=%q err=%v", out.String(), err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed restore published destination", err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".kypost-restore-*"))
	if err != nil || len(stages) != 1 {
		t.Fatal("missing private failed staging", err)
	}
	staging := filepath.Join(stages[0], "data")
	got, err := os.ReadFile(filepath.Join(staging, "config/users.json"))
	if err != nil || !bytes.Equal(got, users) {
		t.Fatal("failed restore erased staging data", err)
	}
	if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(staging, "state")), sso.ErrNativeRestoreHold) {
		t.Fatal("failed restore did not leave hold")
	}
}

// A capsule naming a bulk snapshot never restores without its repository.
func TestRestoreCLIRefusesBulkWithoutRepository(t *testing.T) {
	t.Setenv("KYPOST_BULK_BACKUP_REPOSITORY", "")
	key, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recoverykey.Split(key, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"version":1,"snapshot":"` + strings.Repeat("a", 64) + `","root":"/mail","files":[{"path":"state/receiving/ingress.db","sha256":"` + strings.Repeat("b", 64) + `","size":1}]}`)
	raw, _, err := recoveryclient.Seal(recoveryclient.Payload{ServiceName: backup.AppName, AppVersion: "test", Files: []recoveryclient.File{{Path: "state/mail-bulk.json", Data: manifest, Mode: 0600}}}, recoveryclient.RecoveryKey{Public: key.Public(), Threshold: 2, TotalShares: 3})
	if err != nil {
		t.Fatal(err)
	}
	path, target := filepath.Join(t.TempDir(), "bulk.kycap"), filepath.Join(t.TempDir(), "restore")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runRestore([]string{path, target}, strings.NewReader(shares[0].String()+"\n"+shares[1].String()+"\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "KYPOST_BULK_BACKUP_REPOSITORY") || out.Len() != 0 {
		t.Fatalf("bulk capsule restored without its repository: output=%q err=%v", out.String(), err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused restore published destination", err)
	}
}
