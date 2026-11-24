package backup

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
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableBackupRefusesLocalDestination(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dirs := fixtureDirs(t)
	st, err := state.New(dirs.State)
	must(t, err)
	defer st.Close()
	db, err := volumes.Open(dirs.State, true)
	must(t, err)
	defer db.Close()
	table, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: bytes.Repeat([]byte{8}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	must(t, err)
	unmount, err := vol.Mount(dirs.Config, table)
	must(t, err)
	defer unmount()
	for _, dir := range []string{filepath.Join(dirs.State, "backups"), filepath.Join(t.TempDir(), "capsules")} {
		_, err = New(dirs, config.BackupConfig{Dir: dir, Keep: 1}, st, "test")
		if err == nil || !strings.Contains(err.Error(), "KYPOST_BACKUP_DIR") || !strings.Contains(err.Error(), "KyRecovery") {
			t.Fatal("table backup did not explain local destination refusal", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("refused backup created local destination", err)
		}
	}
	if _, err := st.Setting("backup_last_attempt"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("constructor changed backup attempt", err)
	}
}
