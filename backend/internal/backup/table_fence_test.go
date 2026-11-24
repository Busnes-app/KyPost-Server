package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func tableBackupReplicas(t *testing.T) (*Service, *Service) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	aDirs := Dirs{Config: filepath.Join(root, "host-a-config"), State: filepath.Join(root, "host-a-state"), Secret: filepath.Join(root, "host-a-secret")}
	bDirs := Dirs{Config: filepath.Join(root, "host-b-config"), State: filepath.Join(root, "host-b-state"), Secret: filepath.Join(root, "host-b-secret")}
	t.Setenv("CONFIG_DIR", aDirs.Config)
	t.Setenv("STATE_DIR", aDirs.State)
	dbbackend.FreshDatabaseForTest(t)
	for _, path := range []string{aDirs.Config, aDirs.State, aDirs.Secret, bDirs.Config, bDirs.State, bDirs.Secret} {
		must(t, os.MkdirAll(path, 0700))
	}
	st, err := state.New(aDirs.State)
	must(t, err)
	t.Cleanup(func() { st.Close() })
	db, err := volumes.Open(aDirs.State, true)
	must(t, err)
	t.Cleanup(func() { db.Close() })
	table, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: bytes.Repeat([]byte{4}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	must(t, err)
	for _, dir := range []string{aDirs.Config, bDirs.Config} {
		release, err := vol.Mount(dir, table)
		must(t, err)
		t.Cleanup(release)
	}
	// The two hosts share the logical state store, not a filesystem inode.
	a, err := New(aDirs, config.BackupConfig{DepositInterval: time.Hour}, st, "test")
	must(t, err)
	b, err := New(bDirs, config.BackupConfig{DepositInterval: time.Hour}, st, "test")
	must(t, err)
	return a, b
}

func TestTableBackupOperationExcludesAnotherHost(t *testing.T) {
	a, b := tableBackupReplicas(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	stopped := errors.New("stop before backup side effects")
	go func() {
		_, err := a.RunIfDue(t.Context(), func() error { close(entered); <-release; return stopped })
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("first host did not enter backup", err)
	}
	publicKey := base64.StdEncoding.EncodeToString(newKey(t).Public().Bytes())
	operations := map[string]func() error{
		"run":       func() error { _, err := b.Run(t.Context()); return err },
		"scheduled": func() error { _, err := b.RunIfDue(t.Context(), nil); return err },
		"export":    func() error { _, _, err := b.Export(t.Context()); return err },
		"drill":     func() error { _, err := b.Drill(t.Context()); return err },
		"pair":      func() error { _, err := b.Pair(t.Context(), "http://invalid.example.test", "000000"); return err },
		"pair-if-missing": func() error {
			_, err := b.PairIfMissing(t.Context(), "http://invalid.example.test", "000000")
			return err
		},
		"pin":      func() error { return b.PinKey(publicKey, 2, 3) },
		"unpair":   b.Unpair,
		"interval": func() error { return b.SetIntervalSeconds(3600) },
		"status":   func() error { _, err := b.Status(); return err },
	}
	var wg sync.WaitGroup
	for name, operation := range operations {
		wg.Go(func() {
			if err := operation(); !errors.Is(err, recoveryclient.ErrInProgress) {
				t.Errorf("%s entered another host's operation: %v", name, err)
			}
		})
	}
	wg.Wait()
	close(release)
	if result := <-done; !errors.Is(result, stopped) {
		t.Fatal("held run", result)
	}
	if _, err := a.store.Setting("backup_interval_sec"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("blocked setting changed", err)
	}
	if _, err := a.store.Setting("kyrecovery_key_id"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("blocked pin changed", err)
	}
	if _, err := a.store.Setting("backup_last_attempt"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("blocked run changed attempt", err)
	}
	for _, dir := range []string{a.dirs.State, b.dirs.State} {
		if _, err := os.Stat(filepath.Join(dir, "backup-operation.lock")); !os.IsNotExist(err) {
			t.Fatal("table operation created host-local lock", err)
		}
	}
	must(t, b.SetIntervalSeconds(3600))
	t.Run("cancelled-before-entry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		entered := false
		err := b.withOperation(ctx, func(context.Context) error { entered = true; return nil })
		if !errors.Is(err, context.Canceled) || entered {
			t.Fatal("cancelled operation entered", entered, err)
		}
	})
	t.Run("lost-session", func(t *testing.T) {
		entered := make(chan struct{})
		lost := make(chan error, 1)
		go func() {
			lost <- a.withOperation(t.Context(), func(owned context.Context) error { close(entered); <-owned.Done(); return nil })
		}()
		<-entered
		observer, err := dbbackend.Open(filepath.Join(a.dirs.State, "state.db"), dbbackend.Options{Kind: "state"})
		must(t, err)
		defer observer.Close()
		killedAt := time.Now()
		var killed int
		// This fixture owns an isolated database; no other test's session is targeted.
		must(t, observer.QueryRowContext(t.Context(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND objsubid=1 AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&killed))
		if killed != 1 {
			t.Fatal("expected one operation fence", killed)
		}
		select {
		case err := <-lost:
			if !errors.Is(err, state.ErrOwnershipLost) {
				t.Fatal("lost operation reported success", err)
			}
		case <-time.After(state.ProbeBound() + time.Second):
			t.Fatal("operation ignored lost fence")
		}
		if time.Since(killedAt) > state.ProbeBound()+time.Second {
			t.Fatal("late fence cancellation")
		}
		admitted := false
		must(t, b.withOperation(t.Context(), func(context.Context) error { admitted = true; return nil }))
		if !admitted {
			t.Fatal("another host could not enter after loss")
		}
	})
}
