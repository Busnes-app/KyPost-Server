package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/replica"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// enterTableMode selects table mode for the roots, as a migrated install (MH8) would.
func enterTableMode(t *testing.T, st *state.Store, configDir, stateDir, secretDir string) {
	t.Helper()
	if err := st.SetSetting("volume_stores", "postgres"); err != nil {
		t.Fatal(err)
	}
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	provisionTableKeys(t, secretDir)
	releaseAccounts, err := users.MountAccounts(configDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := users.OpenExisting(context.Background(), configDir)
	if err == nil {
		_, err = accounts.CreateSSOUser("retained-admin", users.RoleAdmin, "", "", "")
	}
	releaseAccounts()
	if err != nil {
		t.Fatal(err)
	}
	release, err := startDocuments(context.Background(), configDir, stateDir, secretDir, config.TenancySingle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// A table-mode replica refuses to start, before it registers or writes anything, while a
// store is still written to the host's disk, and when a root is not on the documents table.
func TestTableModeStartRunsTheVolumeFreeAssertion(t *testing.T) {
	dbbackend.RequirePostgres(t)
	t.Setenv("POW_SECRET", "")
	d := newGracefulShutdownTestDeps(t)
	secret := os.Getenv("SECRET_DIR")
	enterTableMode(t, d.store, d.configDir, d.stateDir, secret)
	_, _, err := startReplica(context.Background(), d, "api")
	if !errors.Is(err, tenancy.ErrHostFiles) {
		t.Fatalf("table-mode start with stores on the host's disk: %v", err)
	}
	live, lerr := d.store.LiveInstances(context.Background())
	if lerr != nil || len(live) != 0 {
		t.Fatalf("a refused start registered %+v (%v)", live, lerr)
	}
	for _, dir := range []string{d.configDir, d.stateDir, secret} {
		if _, err := os.Stat(filepath.Join(dir, replica.VolumeIDFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused start wrote %s in %s: %v", replica.VolumeIDFile, dir, err)
		}
	}
	resolved := map[string]tenancy.Target{"contacts.json": tenancy.TargetDocuments}
	if err := assertVolumeFree(d.configDir, d.stateDir, resolved); err != nil {
		t.Fatalf("both roots on the documents table, no store on disk: %v", err)
	}
	if err := assertVolumeFree(t.TempDir(), d.stateDir, resolved); !errors.Is(err, tenancy.ErrHostFiles) {
		t.Fatalf("a config root off the documents table: %v", err)
	}
	if err := assertVolumeFree(d.configDir, t.TempDir(), resolved); !errors.Is(err, tenancy.ErrHostFiles) {
		t.Fatalf("a state root off the documents table: %v", err)
	}
}
