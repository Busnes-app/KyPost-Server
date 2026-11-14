package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// runMigrateNative is run by entrypoint.sh as the runtime user before any
// service starts: storage format (v1 -> v2 -> v3), then the deployment's
// mailbox limits. A failure leaves native mail and SSO refused, never
// password logins or IMAP.
func runMigrateNative(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: kypost-server migrate-native")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	migrated, err := sso.MigrateNative(ctx, config.ConfigDir(), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err == nil {
		// Then v2 -> v3 (native-authority.db), every SSO install included.
		var moved bool
		moved, err = sso.MigrateAuthority(ctx, config.ConfigDir())
		migrated = migrated || moved
	}
	if err == nil {
		var keyed bool
		keyed, err = reconcileDKIMKeys(ctx, config.ConfigDir(), filepath.Join(config.SecretDir(), "native-relay.key"))
		migrated = migrated || keyed
	}
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
		return fmt.Errorf("native mail storage migration failed; native mail and SSO sign-in stay refused, password logins and external IMAP are unaffected. Keep the config volume and its *%s and *%s copies, fix the cause below and restart, or restore the pre-migration backup: %w", sso.NativeMigrationCopySuffix, sso.NativeAuthorityCopySuffix, err)
	}
	return nil
}

// reconcileDKIMKeys gives a direct profile saved before DKIM signing a key per
// sending domain, under the native domain fence the admin save path holds.
// Relay damage is left to the runtime reader, which refuses it the same way;
// it must not block SSO sign-in.
func reconcileDKIMKeys(ctx context.Context, configDir, keyPath string) (bool, error) {
	path := filepath.Join(configDir, "native-relay.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(configDir, sso.NativeDomainsFile))
	if err != nil {
		return false, err
	}
	defer release()
	keyed, err := mailmsg.ReconcileDKIMKeys(ctx, path, keyPath)
	if errors.Is(err, mailmsg.ErrDomainRelay) {
		return false, nil
	}
	return keyed, err
}
