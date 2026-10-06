package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// runMigrateNative is run by entrypoint.sh as the runtime user before any
// service starts. A failure leaves native mail refused, never IMAP.
func runMigrateNative(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: kypost-server migrate-native")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	migrated, err := sso.MigrateNative(ctx, config.ConfigDir(), filepath.Join(config.SecretDir(), "native-relay.key"))
	result := "unchanged"
	if err != nil {
		result = "failed"
	} else if migrated {
		result = "migrated"
	}
	slog.Info("native storage migration", "actor", "entrypoint", "task_id", "migrate-native", "action", "migrate", "target", "native-config", "result", result)
	if err != nil {
		return fmt.Errorf("native mail storage migration failed; native mail stays refused and external IMAP is unaffected. Keep the config volume and its *%s copies, fix the cause below and restart, or restore the pre-migration backup: %w", sso.NativeMigrationCopySuffix, err)
	}
	return nil
}
