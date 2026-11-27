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

// With every store off the host's disk (MH7), a table-mode replica starts and registers
// itself without writing replica-volume.id on disk; assertVolumeFree still refuses any
// store marked TargetFile or any root off the documents table.
func TestTableModeStartRunsTheVolumeFreeAssertion(t *testing.T) {
	dbbackend.RequirePostgres(t)
	t.Setenv("POW_SECRET", "")
	d := newGracefulShutdownTestDeps(t)
	secret := os.Getenv("SECRET_DIR")
	enterTableMode(t, d.store, d.configDir, d.stateDir, secret)
	instance, release, err := startReplica(context.Background(), d, "api")
	if err != nil {
		t.Fatalf("table-mode start failed: %v", err)
	}
	defer release()
	if instance == "" {
		t.Fatal("table-mode start returned an empty instance id")
	}
	live, lerr := d.store.LiveInstances(context.Background())
	if lerr != nil || len(live) != 1 || live[0].ID != instance {
		t.Fatalf("table-mode start live instances = %+v (%v), want [%s]", live, lerr, instance)
	}
	for _, dir := range []string{d.configDir, d.stateDir, secret} {
		if _, err := os.Stat(filepath.Join(dir, replica.VolumeIDFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("table-mode start wrote %s in %s: %v", replica.VolumeIDFile, dir, err)
		}
	}
	if err := assertVolumeFree(d.configDir, d.stateDir, map[string]tenancy.Target{"on-disk.json": tenancy.TargetFile}); !errors.Is(err, tenancy.ErrHostFiles) {
		t.Fatalf("a TargetFile store must refuse start: %v", err)
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
