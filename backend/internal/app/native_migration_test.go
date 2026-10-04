//go:build linux

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeAdminDoesNotReceiveLegacyMigration(t *testing.T) {
	logger, accounts, configDir, stateDir, oldAdmin := legacyMigrationEnv(t)
	owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "native-admin", Mailbox: "native-admin-id"}
	u, err := accounts.PublishPreparedSSOUser(context.Background(), owner.Mailbox, "native-admin", users.RoleAdmin, owner.Issuer, owner.Subject, "native-admin", "admin@example.test", func() (string, error) {
		return mailbox.PrepareAccount(stateDir, owner, "admin@example.test", mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Deactivate(oldAdmin); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacySingleUserData(logger, accounts, configDir, stateDir, config.UserNotificationSettings{}, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(configDir, "users", u.ID, "imap-config.json"),
		filepath.Join(stateDir, "users", u.ID, "state.json"),
		filepath.Join(stateDir, "users", u.ID, "decisions.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("native admin inherited legacy file %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(configDir, "imap-config.json")); err != nil {
		t.Fatalf("skipped migration consumed legacy credential: %v", err)
	}
}
