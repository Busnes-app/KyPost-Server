package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// runMigrateNative is run by entrypoint.sh as the runtime user before any
// service starts: storage format, then the deployment's mailbox limits. A
// failure leaves native mail refused, never IMAP.
func runMigrateNative(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: kypost-server migrate-native")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	migrated, err := sso.MigrateNative(ctx, config.ConfigDir(), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err == nil {
		var quota int64
		if quota, err = config.MailboxQuotaBytes(); err == nil {
			var raised bool
			// Its own deadline, scaled to the mailbox count.
			raised, err = sso.MigrateNativeLimits(context.Background(), config.ConfigDir(), mailbox.NativeLimits(quota))
			migrated = migrated || raised
		}
	}
	result := "unchanged"
	if err != nil {
		result = "failed"
	} else if migrated {
		result = "migrated"
	}
	slog.Info("native storage migration", "actor", "entrypoint", "task_id", "migrate-native", "action", "migrate", "target", "native-config", "result", result)
	switch {
	case errors.Is(err, sso.ErrNativeMailboxLimits):
		return fmt.Errorf("native mailbox limits: %w", err)
	case err != nil:
		return fmt.Errorf("native mail storage migration failed; native mail stays refused and external IMAP is unaffected. Keep the config volume and its *%s copies, fix the cause below and restart, or restore the pre-migration backup: %w", sso.NativeMigrationCopySuffix, err)
	}
	return nil
}
