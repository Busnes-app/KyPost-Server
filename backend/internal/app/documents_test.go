package app

import (
	"bytes"
	"context"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTableModeSelectionAndProvisionedKey(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	configDir, stateDir, secretDir := filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "private")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if exists, err := dbbackend.Exists(filepath.Join(stateDir, volumes.File)); err != nil || exists {
		t.Fatal("ordinary install gained volumes store", exists, err)
	}
	if err := st.SetSetting("volume_stores", "postgres"); err != nil {
		t.Fatal(err)
	}
	if _, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle); err == nil {
		t.Fatal("table mode accepted missing master key")
	}
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	provisionTableKeys(t, secretDir)
	releaseAccountFixture, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	accountFixture, err := users.OpenExisting(context.Background(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = accountFixture.CreateSSOUser("retained-admin", users.RoleAdmin, "", "", ""); err != nil {
		t.Fatal(err)
	}
	releaseAccountFixture()
	release, err = startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := filepath.Join(configDir, "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatal("table config created local directory", err)
	}
}

func TestTableModeRefusesHistoricalFileAccountAuthority(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	configDir, stateDir, secretDir := filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(root, "private")
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
	if err = os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "users.json")
	before := []byte(`{"version":1,"users":[{"id":"retained-admin","username":"retained-admin","role":"admin","active":true}]}`)
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
	if err == nil {
		release()
		t.Fatal("historical file accounts silently replaced by empty table")
	}
	if !strings.Contains(err.Error(), "users.json") {
		t.Fatal("refusal does not identify retained authority", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed file accounts", err)
	}
	if users.TableMode(configDir) {
		t.Fatal("refused startup left accounts mounted")
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("refusal changed table accounts", count, err)
	}
}
