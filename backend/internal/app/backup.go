package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

var backupSubcommands = map[string]bool{"backup-drill": true, "export-capsule": true, "deposit": true, "restore": true}

// backupSubcommand recognises the four backup verbs, which come before any
// -mode flag: they run once and exit, never as a supervised process.
func backupSubcommand(args []string) (name string, rest []string, ok bool) {
	if len(args) == 0 || !backupSubcommands[args[0]] {
		return "", nil, false
	}
	return args[0], args[1:], true
}

// newBackupService opens the install-wide store and binds the adapter, for
// the CLI verbs and the daemon loop alike.
func newBackupService(store *state.Store) (*backup.Service, error) {
	bc, err := config.LoadBackupConfig()
	if err != nil {
		return nil, err
	}
	return backup.New(backup.Dirs{Config: config.ConfigDir(), State: config.StateDir(), Secret: config.SecretDir()}, bc, store, api.Version())
}

// runBackupCommand executes one verb and returns its exit error.
func runBackupCommand(name string, rest []string, stdin io.Reader, stdout io.Writer) error {
	if (name == "backup-drill" || name == "deposit") && len(rest) != 0 {
		return fmt.Errorf("usage: kypost-server %s", name)
	}
	if name == "export-capsule" && len(rest) != 1 {
		return errors.New("usage: kypost-server export-capsule <out.kycap>")
	}
	if name == "restore" {
		return runRestore(rest, stdin, stdout)
	}
	st, err := state.New(config.StateDir())
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	defer st.Close()
	svc, err := newBackupService(st)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), svc.RunBudget())
	defer cancel()
	if err := svc.Audit("admin.backup_intent", "cli", name, "started", nil); err != nil {
		return err
	}
	switch name {
	case "backup-drill":
		res, err := svc.Drill(ctx)
		details := map[string]any{}
		if err != nil {
			details["error"] = recoveryclient.AuditSafe(err.Error())
		} else {
			details["checks"] = res.Checks
		}
		if auditErr := svc.Audit("admin.backup_drill", "cli", "", outcomeWord(err == nil && res != nil && res.Passed), details); auditErr != nil {
			return fmt.Errorf("drill completion audit failed: %w", auditErr)
		}
		if err != nil {
			return err
		}
		for _, c := range res.Checks {
			fmt.Fprintf(stdout, "%-40s %v %s\n", c.Name, c.Passed, c.Message)
		}
		if !res.Passed {
			return errors.New("drill failed")
		}
		return nil
	case "export-capsule":
		raw, m, err := svc.Export(ctx)
		details := map[string]any{"path": recoveryclient.AuditSafe(rest[0])}
		if err != nil {
			details["error"] = recoveryclient.AuditSafe(err.Error())
		}
		if auditErr := svc.Audit("admin.backup_export", "cli", m.CapsuleID, outcomeWord(err == nil), details); auditErr != nil {
			return fmt.Errorf("export audit failed; no bytes released: %w", auditErr)
		}
		if err != nil {
			return err
		}
		f, err := os.OpenFile(rest[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		return errors.Join(err, f.Close())
	case "deposit":
		res, err := svc.Run(ctx)
		action, outcome, details := recoveryclient.Outcome(res, err)
		if auditErr := svc.Audit(action, "cli", res.Manifest.CapsuleID, outcome, details); auditErr != nil {
			return fmt.Errorf("backup may have completed; completion audit failed: %w", auditErr)
		}
		if err != nil && !errors.Is(err, recoveryclient.ErrReceiptUnrecorded) {
			return err
		}
		fmt.Fprintf(stdout, "capsule %s (%d bytes)", res.Manifest.CapsuleID, res.SizeBytes)
		if res.LocalPath != "" {
			fmt.Fprintf(stdout, " local %s", res.LocalPath)
		}
		if res.Receipt != nil {
			fmt.Fprintf(stdout, " deposited %s", res.Receipt.DepositedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(stdout)
		if err != nil {
			fmt.Fprintln(stdout, "warning: KyRecovery holds the capsule but the receipt was not recorded here")
		}
		return nil
	}
	return fmt.Errorf("unknown backup command %q", name)
}

func outcomeWord(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

// runRestore reads k custodian shares from stdin, one per line, and never from
// argv: a share in argv is in shell history and /proc/<pid>/cmdline.
func runRestore(rest []string, stdin io.Reader, stdout io.Writer) error {
	if len(rest) != 2 {
		return errors.New("usage: kypost-server restore <capsule.kycap> <target-dir>  (shares on stdin, one per line; never in argv)")
	}
	bc, err := config.LoadBackupConfig()
	if err != nil {
		return err
	}
	// The recipe is unauthenticated until the capsule opens; it only lets a
	// restore that would fail anyway fail before custodians type shares.
	raw, err := os.ReadFile(rest[0])
	if err != nil {
		return err
	}
	if peek, err := capsule.ReadUnverifiedManifest(raw); err == nil && bc.BulkRepository == "" {
		if recipe, _ := peek.VerificationRecipe.(map[string]any); recipe["bulk"] != nil {
			return errors.New("this capsule's mail is in a restic bulk snapshot; set KYPOST_BULK_BACKUP_REPOSITORY to its repository before entering shares")
		}
	}
	shares, err := recoveryclient.ReadShares(stdin)
	if err != nil {
		return err
	}
	// Delay the library's success summary until product validation and hold persist.
	var summary bytes.Buffer
	target := filepath.Clean(rest[1])
	// The supplied destination never exposes partially extracted native state.
	// Keep failed staging for inspection; it is never the configured live root.
	stage, err := os.MkdirTemp(filepath.Dir(target), ".kypost-restore-")
	if err != nil {
		return err
	}
	staging := filepath.Join(stage, "data")
	if err := recoveryclient.Restore(rest[0], staging, backup.AppName, shares, &summary); err != nil {
		return fmt.Errorf("restore failed; preserve private staging %s for inspection: %w", stage, err)
	}
	// Mail databases from a bulk snapshot are placed before any validation runs.
	if err := backup.RestoreBulk(context.Background(), staging, bc.BulkRepository, bc.ResticBinary); err != nil {
		return fmt.Errorf("bulk restore failed; preserve private staging %s for inspection: %w", stage, err)
	}
	native, err := backup.QuarantineNativeRestore(staging)
	if err != nil {
		return fmt.Errorf("restore validation failed; preserve private staging %s for inspection: %w", stage, err)
	}
	if native {
		err = fsutil.PublishDirectory(staging, target)
	} else {
		// Preserve the legacy empty-directory restore contract on other platforms.
		entries, readErr := os.ReadDir(target)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			err = readErr
		} else if len(entries) != 0 {
			err = errors.New("restore target is occupied")
		} else {
			err = os.Rename(staging, target)
		}
	}
	if err != nil {
		return fmt.Errorf("restore publication refused; destination retained and verified staging at %s: %w", stage, err)
	}
	if err := fsutil.SyncDir(filepath.Dir(target)); err != nil {
		return fmt.Errorf("restored data published but directory sync failed; keep workers stopped: %w", err)
	}
	if err := os.Remove(stage); err != nil {
		return err
	}
	if _, err := io.Copy(stdout, &summary); err != nil {
		return err
	}
	if native {
		_, err = fmt.Fprintln(stdout, "Native mail remains held: reconcile current identity, DNS and receiver evidence before resuming workers.")
	}
	return err
}

// backupLoop polls the admin's schedule every minute in the daemon process and
// runs when due. It never exits on error; a dead destination is retried once
// per interval because Run stamps the attempt first.
func backupLoop(ctx context.Context, svc *backup.Service, logger *logging.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		configured, err := svc.Configured()
		if err != nil {
			logger.Error("backup configuration unreadable")
			continue
		}
		if !configured {
			continue
		}
		next, on, err := svc.NextRun()
		if err != nil {
			logger.Error("backup schedule unreadable", "error", recoveryclient.AuditSafe(err.Error()))
			continue
		}
		if !on || time.Now().Before(next) {
			continue
		}
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), svc.RunBudget())
		if err := svc.Audit("admin.backup_intent", "kypost-scheduler", "run", "started", nil); err != nil {
			cancel()
			logger.Error("backup intent audit unavailable")
			continue
		}
		res, err := svc.Run(runCtx)
		cancel()
		action, outcome, details := recoveryclient.Outcome(res, err)
		if aerr := svc.Audit(action, "kypost-scheduler", res.Manifest.CapsuleID, outcome, details); aerr != nil {
			logger.Error("backup audit write failed", "error", aerr.Error())
		}
		if err != nil && !errors.Is(err, recoveryclient.ErrReceiptUnrecorded) {
			logger.Error("scheduled backup failed", "error", recoveryclient.AuditSafe(err.Error()))
			continue
		}
		logger.Info("scheduled backup done", "capsule_id", res.Manifest.CapsuleID, "bytes", strconv.Itoa(res.SizeBytes))
	}
}

func startBackupLoop(ctx context.Context, d runDeps) (<-chan struct{}, error) {
	svc, err := newBackupService(d.store)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { defer close(done); backupLoop(ctx, svc, d.logger) }()
	return done, nil
}
