package sso

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableCurrentSettingsFencesAnotherHostSave(t *testing.T) {
	dbbackend.RequirePostgres(t)
	configDir, stateDir, otherDir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CONFIG_DIR", configDir)
	t.Setenv("STATE_DIR", stateDir)
	dbbackend.FreshDatabaseForTest(t)
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, err := volumes.Open(stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, err := vol.NewTable(db, "config", vol.TableOptions{MasterKey: bytes.Repeat([]byte{6}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{configDir, otherDir} {
		release, err := vol.Mount(dir, table)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	a, b := NewStore(configDir), NewStore(otherDir)
	if err := a.Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.WithCurrentSettings(t.Context(), func(ctx context.Context, current SSOSettings) error {
			if !current.Enabled {
				t.Error("missing current settings")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	saved := make(chan error, 1)
	go func() { saved <- b.Save(SSOSettings{}) }()
	observer, err := dbbackend.Open(filepath.Join(stateDir, "state.db"), dbbackend.Options{Kind: "state"})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	waiting, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer waitCancel()
	for {
		select {
		case err := <-saved:
			close(release)
			<-done
			t.Fatal("another host changed settings during authority commit", err)
		default:
		}
		var queued int
		err := observer.QueryRowContext(waiting, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objsubid=1 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&queued)
		if err != nil {
			close(release)
			<-done
			t.Fatal("settings writer did not queue", err)
		}
		if queued == 1 {
			break
		}
		select {
		case <-waiting.Done():
			close(release)
			<-done
			t.Fatal("settings writer never waited")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if a.Load().Enabled {
		t.Fatal("later settings save was not visible")
	}
	t.Run("legacy-omitted-fields-retain-file-meaning", func(t *testing.T) {
		if err := vol.OpenDocument(a.path).Replace([]byte(`{"enabled":true}`)); err != nil {
			t.Fatal(err)
		}
		err := a.WithCurrentSettings(t.Context(), func(_ context.Context, current SSOSettings) error {
			if !current.Enabled || current.AutoProvision {
				t.Fatal("stored settings defaults changed", current)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("corrupt-settings-refuse-callback", func(t *testing.T) {
		if err := vol.OpenDocument(a.path).Replace([]byte("corrupt settings")); err != nil {
			t.Fatal(err)
		}
		entered := false
		err := a.WithCurrentSettings(t.Context(), func(context.Context, SSOSettings) error { entered = true; return nil })
		if err == nil || entered {
			t.Fatal("corrupt settings became empty authority", entered, err)
		}
		if err := a.Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing-instance-refuses-without-fallback", func(t *testing.T) {
		missing := filepath.Join(stateDir, "missing-instance")
		bad := NewStoreWithState(configDir, missing)
		if err := bad.Save(SSOSettings{}); err == nil {
			t.Fatal("missing instance fence admitted save")
		}
		if !a.Load().Enabled {
			t.Fatal("failed save changed settings")
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("fence opener created local instance", err)
		}
	})
	t.Run("cancelled-before-entry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		entered := false
		err := a.WithCurrentSettings(ctx, func(context.Context, SSOSettings) error { entered = true; return nil })
		if !errors.Is(err, context.Canceled) || entered {
			t.Fatal("cancelled admission entered", entered, err)
		}
	})
	t.Run("lost-settings-fence-cancels-commit-context", func(t *testing.T) {
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- a.WithCurrentSettings(t.Context(), func(owned context.Context, _ SSOSettings) error { close(entered); <-owned.Done(); return nil })
		}()
		<-entered
		var killed int
		err := observer.QueryRowContext(t.Context(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND objsubid=1 AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&killed)
		if err != nil || killed != 1 {
			t.Fatal("expected only this isolated cell's SSO fence", killed, err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, state.ErrOwnershipLost) {
				t.Fatal("lost fence reported success", err)
			}
		case <-time.After(state.ProbeBound() + time.Second):
			t.Fatal("lost settings fence did not cancel action")
		}
		if err := b.Save(SSOSettings{}); err != nil {
			t.Fatal("later host could not save", err)
		}
	})
	for _, dir := range []string{configDir, otherDir} {
		if _, err := os.Stat(filepath.Join(dir, "sso.json.lock")); !os.IsNotExist(err) {
			t.Fatal("table authority created local settings lock", err)
		}
	}
}
