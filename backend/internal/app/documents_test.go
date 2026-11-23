package app

import (
	"context"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
	"os"
	"path/filepath"
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
	if _, err := cryptutil.LoadOrCreateKey(filepath.Join(secretDir, "documents.key")); err != nil {
		t.Fatal(err)
	}
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
