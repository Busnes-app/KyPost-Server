package app

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// A Postgres install whose server is unreachable never falls back to the SQLite files
// a migration left at the live paths: every opener of an existing database fails, and
// the files are not touched.
func TestUnreachablePostgresNeverReadsLiveSQLiteFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("CONFIG_DIR", filepath.Join(root, "config"))
	var stateDir, configDir, source string
	t.Run("files a migration left behind", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		stateDir, configDir, source = migrateFixture(t, root)
		if _, err := sso.NewLifecycleStore(configDir).ApplyDirectory("https://identity.example.test", syncauth.Event{ID: "e1", Type: "user.updated", At: time.Now()}, "s", 1, "digest", true, func() (bool, error) { return false, nil }); err != nil {
			t.Fatal(err)
		}
	})
	hashes := databaseFiles(t, stateDir, configDir)
	dbbackend.SetURLForTest(t, "postgres://kypost:pw@127.0.0.1:1/kypost?connect_timeout=1")
	ctx := context.Background()
	account := filepath.Join(stateDir, "users", "one")
	checks := map[string]func() error{
		"state.OpenNative": func() error { _, err := state.OpenNative(account, source); return err },
		"mailbox.OpenExisting": func() error {
			_, err := mailbox.OpenExisting(filepath.Join(account, "mailbox"), migrateOwner, migrateLimits, source)
			return err
		},
		"mailbox.ValidatePreparedAccount": func() error {
			_, err := mailbox.ValidatePreparedAccount(stateDir, migrateOwner, "one@example.test", migrateLimits)
			return err
		},
		"mailbox.HasPendingOutbound": func() error {
			_, err := mailbox.HasPendingOutbound(ctx, filepath.Join(account, "mailbox"))
			return err
		},
		"ingress.OpenExisting": func() error {
			_, err := ingress.OpenExisting(filepath.Join(stateDir, "receiving"), ingress.ReceivingLimits)
			return err
		},
		"sso authority read": func() error {
			_, _, err := sso.NewLifecycleStore(configDir).NativeAssignment("https://identity.example.test", "s")
			return err
		},
		"sso authority write": func() error {
			_, err := sso.NewLifecycleStore(configDir).ApplyDirectory("https://identity.example.test", syncauth.Event{ID: "e2", Type: "user.updated", At: time.Now()}, "s", 2, "digest2", true, func() (bool, error) { return false, nil })
			return err
		},
		"state.SnapshotDBToFile": func() error {
			return state.SnapshotDBToFile(ctx, filepath.Join(stateDir, "state.db"), filepath.Join(t.TempDir(), "snap.db"))
		},
		"state.FenceRestoredScheduledSends": func() error { return state.FenceRestoredScheduledSends(stateDir) },
		"state.FenceRestoredAgentTokens":    func() error { return state.FenceRestoredAgentTokens(stateDir) },
		"mailbox.RotateRestoredMessageReferences": func() error {
			return mailbox.RotateRestoredMessageReferences(filepath.Join(account, "mailbox", "mailbox.db"), source)
		},
		"mailbox.ValidateOutboundSnapshot": func() error {
			_, err := mailbox.ValidateOutboundSnapshot(ctx, filepath.Join(account, "mailbox", "mailbox.db"), nil, mailmsg.DomainRelay{})
			return err
		},
		"mailbox.InspectRestored": func() error {
			_, _, err := mailbox.InspectRestored(filepath.Join(account, "mailbox", "mailbox.db"))
			return err
		},
		"sso restore qualification": func() error {
			_, err := sso.NewLifecycleStore(configDir).CheckNativeRestoreQualification(stateDir)
			return err
		},
		"dbbackend.OpenFile": func() error {
			db, err := dbbackend.OpenFile(filepath.Join(account, "state.db"), "mode=rw")
			if err == nil {
				err = db.Ping()
				_ = db.Close()
			}
			return err
		},
	}
	for name, open := range checks {
		if err := open(); err == nil {
			t.Errorf("%s succeeded with Postgres unreachable", name)
		}
	}
	if got := databaseFiles(t, stateDir, configDir); !reflect.DeepEqual(got, hashes) {
		t.Fatal("a live SQLite file was written while Postgres was unreachable")
	}
}
