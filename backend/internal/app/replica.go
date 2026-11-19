package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/replica"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/worker"
)

// startReplica runs the PostgreSQL replica checks for this process and registers it as a
// live instance until release: one volume set, one host, no Maddy receiver, one shared
// configuration and a persisted proof-of-work key. Nothing listens or works before it
// returns. On SQLite it does nothing; the server-instance lock is taken in Run.
func startReplica(ctx context.Context, d runDeps, role string) (id string, release func(), err error) {
	if !dbbackend.Postgres() {
		return "", func() {}, nil
	}
	boot, err := replica.ReadBootID()
	if err != nil {
		return "", nil, err
	}
	id, release, err = replica.Start(ctx, replica.Process{
		Store: d.store, Role: role, BootID: boot, Getenv: os.Getenv,
		Roots: []string{d.configDir, config.SecretDir(), d.stateDir},
	})
	if err != nil {
		return "", nil, err
	}
	totp := config.SecretFile("TOTP_SECRET_KEY_FILE", "totp-secret.key")
	if _, err = cryptutil.LoadOrCreateKey(totp); err == nil {
		err = replica.CheckConfig(ctx, d.store, totp)
	}
	if err == nil {
		err = api.CheckSharedPoWKey()
	}
	if err == nil {
		// A replica whose recovery.pub went missing rewrites it from the sealed record; a
		// mismatch is reported here and refuses every run until it is resolved.
		if svc, serr := newBackupService(d.store); serr == nil {
			if serr := svc.SyncRecoveryKey(); serr != nil {
				d.logger.Error("recovery key check failed; backups are refused until it is resolved", "error", serr.Error())
			}
		}
	}
	if err != nil {
		release()
		return "", nil, err
	}
	return id, release, nil
}

// electionOptions are the role elections' timings: on PostgreSQL a new owner waits longer
// than the lock's probe bound before its first job; a flock cannot be lost.
func electionOptions() worker.Options {
	if dbbackend.Postgres() {
		return worker.Options{Retry: 5 * time.Second, Grace: state.ProbeBound() + time.Second}
	}
	return worker.Options{Retry: 5 * time.Second}
}

// elect runs work as role's owner for as long as this process holds the role, on its own
// goroutine; wait returns once the election has ended after ctx.
func elect(ctx context.Context, d runDeps, role string, work func(context.Context)) (wait func()) {
	var wg sync.WaitGroup
	wg.Go(func() {
		worker.Run(ctx, d.store, role, electionOptions(), []worker.Job{{Name: role, Run: work}})
	})
	return wg.Wait
}

// runMailRuntime is the mail-runtime role: native outbox recovery and DKIM rotation. Request
// path sends and held-send recovery stay where they are; the outbox claim fences them.
func runMailRuntime(owned context.Context, d runDeps) {
	outbound := startNativeOutbound(owned, d)
	dkim := startDKIMRotation(owned, d)
	<-outbound
	<-dkim
}

// coordinationCleanupInterval is how often the elected api sweeper deletes expired shared
// rows: every 10 minutes keeps a spray of keys from piling up.
const coordinationCleanupInterval = 10 * time.Minute

// cleanCoordination deletes expired shared windows, challenges, replay IDs, sessions and
// one-time authority, on PostgreSQL.
func cleanCoordination(ctx context.Context, st *state.Store, report func(error)) {
	if !dbbackend.Postgres() {
		return
	}
	t := time.NewTicker(coordinationCleanupInterval)
	defer t.Stop()
	for {
		if err := st.CleanExpiredCoordination(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// holdSQLiteInstance takes the shared server-instance lock beside the global state.db, so
// migrate-to-postgres refuses while this process runs. Only the empty STATE_DIR is created
// first; the lock is taken before anything is written in it.
func holdSQLiteInstance(stateDir string) (release func(), err error) {
	if dbbackend.Postgres() {
		return func() {}, nil
	}
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, err
		}
	}
	release, err = state.HoldInstance(stateDir, false)
	if err != nil {
		return nil, fmt.Errorf("server-instance lock: %w", err)
	}
	return release, nil
}

// runAcceptReplicaConfig records this process's shared settings as the deployment's after a
// deliberate change: PostgreSQL only, with no replica running, and only under the same
// totp-secret.key. It prints and audits the names of the settings that changed, never a
// value.
func runAcceptReplicaConfig(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		_, err := fmt.Fprintln(stdout, "usage: kypost-server accept-replica-config\n\nRecords this container's shared settings as the deployment's after you changed one on purpose.\nStop every replica first (docker compose stop kypost-server), then run it once with the new settings.\nIt prints the names of the settings that changed, never their values.")
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			return err
		}
		return errors.New("accept-replica-config takes no arguments")
	}
	if !dbbackend.Postgres() {
		return errors.New("accept-replica-config is for PostgreSQL replicas; a SQLite install has no shared configuration record")
	}
	ctx := context.Background()
	if err := dbbackend.Check(ctx); err != nil {
		return err
	}
	st, err := state.New(config.StateDir())
	if err != nil {
		return err
	}
	defer st.Close()
	live, err := st.LiveInstances(ctx)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("%d replica process(es) are running; stop every replica first (docker compose stop kypost-server), then run this again", len(live))
	}
	changed, err := replica.Accept(ctx, st, config.SecretFile("TOTP_SECRET_KEY_FILE", "totp-secret.key"))
	if err != nil {
		return err
	}
	slog.Info("replica configuration accepted", "action", "admin.replica_config_accepted", "detail", strings.Join(changed, ","))
	if len(changed) == 0 {
		_, err = fmt.Fprintln(stdout, "Recorded this container's shared settings; none differed from the record.")
		return err
	}
	_, err = fmt.Fprintf(stdout, "Recorded this container's shared settings. Changed: %s\n", strings.Join(changed, ", "))
	return err
}

// checkpointSQLite folds every non-empty SQLite write-ahead log under roots back into its
// database at a clean stop. The stores keep their handles open until exit, so without it a
// stopped install leaves -wal files, which migrate-to-postgres rightly refuses. Run after the
// work has stopped, under the instance lock. ponytail: walks the roots once per stop.
func checkpointSQLite(logger *logging.Logger, roots ...string) {
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".db-wal") {
				return nil
			}
			if info, err := d.Info(); err != nil || info.Size() == 0 {
				return nil
			}
			db, err := dbbackend.OpenSQLiteFile(strings.TrimSuffix(p, "-wal"), "_pragma=busy_timeout(5000)")
			if err == nil {
				_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
				err = errors.Join(err, db.Close())
			}
			if err != nil {
				logger.Warn("a SQLite write-ahead log was left at stop", "path", p, "error", err.Error())
			}
			return nil
		})
	}
}
