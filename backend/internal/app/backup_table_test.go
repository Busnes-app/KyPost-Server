package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableBackupCLISelectsDocumentsBeforeService(t *testing.T) {
	dbbackend.FreshDatabaseForTest(t)
	configDir, stateDir, secretDir := t.TempDir(), t.TempDir(), t.TempDir()
	localDir := filepath.Join(t.TempDir(), "capsules")
	for key, value := range map[string]string{"CONFIG_DIR": configDir, "STATE_DIR": stateDir, "SECRET_DIR": secretDir, "KYPOST_BACKUP_DIR": localDir, "KYPOST_TENANCY": "single", "KYPOST_STAFF_ORG_ID": ""} {
		t.Setenv(key, value)
	}
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.SetSetting("volume_stores", "postgres"); err != nil {
		t.Fatal(err)
	}
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	provisionTableKeys(t, secretDir)
	releaseAccounts, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := users.OpenExisting(context.Background(), configDir)
	if err != nil {
		releaseAccounts()
		t.Fatal(err)
	}
	_, err = accounts.CreateSSOUser("retained-admin", users.RoleAdmin, "", "", "")
	releaseAccounts()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runBackupCommand("deposit", nil, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "KYPOST_BACKUP_DIR") {
		t.Fatal("CLI bypassed table backup policy", err)
	}
	if users.TableMode(configDir) || vol.OpenDocument(filepath.Join(configDir, "config.yaml")).IsTable() {
		t.Fatal("refused command retained mounts")
	}
	if _, err := os.Stat(localDir); !os.IsNotExist(err) {
		t.Fatal("refused command created local capsules", err)
	}
	if audit, err := st.RecentBackupAudit(10); err != nil || len(audit) != 0 {
		t.Fatal("refused configuration started a backup", audit, err)
	}
	if _, err := st.Setting("backup_last_attempt"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("refused configuration changed schedule", err)
	}
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KY_CELLS", "test-1")
	t.Setenv("KYPOST_STAFF_ORG_ID", "00000000-0000-0000-0000-000000000001")
	if err := runBackupCommand("deposit", nil, strings.NewReader(""), &output); !errors.Is(err, config.ErrMultiTenancyUnavailable) {
		t.Fatal("CLI lifted hosted startup gate", err)
	}
	t.Setenv("KYPOST_TENANCY", "single")
	t.Setenv("KYPOST_STAFF_ORG_ID", "")
	// The CLI must retain the same historical-account safety refusal as startup.
	retained := []byte(`{"users":[{"id":"historical-owner","role":"admin","active":true}]}`)
	accountPath := filepath.Join(configDir, "users.json")
	if err := os.WriteFile(accountPath, retained, 0600); err != nil {
		t.Fatal(err)
	}
	err = runBackupCommand("deposit", nil, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "users.json") {
		t.Fatal("CLI ignored historical account authority", err)
	}
	after, err := os.ReadFile(accountPath)
	if err != nil || !bytes.Equal(after, retained) {
		t.Fatal("refusal changed retained authority", err)
	}
}
