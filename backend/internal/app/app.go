package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/health"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/kydrive"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/processor"
	"github.com/Busnes-app/kypost-server/backend/internal/sorter"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/wkdpublish"
	"github.com/Busnes-app/kypost-server/backend/internal/worker"
)

// Run dispatches the process mode and blocks until shutdown for long-running modes.
func Run(args []string) error {
	if len(args) > 0 && args[0] == "migrate-to-postgres" {
		return runMigrateToPostgres(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "accept-replica-config" {
		return runAcceptReplicaConfig(args[1:], os.Stdout)
	}
	// Before any store opens: a Postgres configuration that cannot be used refuses
	// every command instead of falling back to SQLite.
	if err := dbbackend.Check(context.Background()); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "setup-status" {
		return runSetupStatus(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "apply-setup" {
		return runApplySetup(args[1:], os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "receiving" {
		if len(args) > 2 && args[1] == "cloudflare" && slices.Contains([]string{"init", "rotate", "takeover", "status"}, args[2]) {
			return runCloudflareContinuous(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "cloudflare" {
			return runCloudflareReceiving(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "quarantine" {
			return runReceivingQuarantine(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "blocks" {
			return runReceivingBlocks(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "config" {
			return runReceivingConfig(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "gateway" {
			return runReceivingGateway(args[2:], os.Stdout)
		}
		if len(args) > 1 && args[1] == "serve" {
			return runReceivingServe(args[2:])
		}
		return runReceivingCommand(args[1:], os.Stdin)
	}
	if len(args) > 0 && args[0] == "migrate-native" {
		return runMigrateNative(args[1:])
	}
	if name, rest, ok := backupSubcommand(args); ok {
		return runBackupCommand(name, rest, os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("kypost-server", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process mode: daemon, server, all, bootstrap-admin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Handled before any of the setup below: bootstrap-admin runs once, as
	// root, before any service starts, and its whole job is to create the
	// account store that the setup below expects to already exist.
	if *mode == "bootstrap-admin" {
		return BootstrapAdmin()
	}

	// SQLite: the shared server-instance lock, before anything below writes to the data
	// roots, so migrate-to-postgres refuses while this process runs.
	releaseInstance, err := holdSQLiteInstance(config.StateDir())
	if err != nil {
		return err
	}
	defer releaseInstance()

	if err = config.CheckEnv(); err != nil {
		return err
	}
	if _, err := config.LoadBackupConfig(); err != nil {
		return err
	}
	nativeMail, err := config.NativeMailEnabled()
	if err != nil {
		return err
	}
	nativeReceiving, err := config.NativeReceivingEnabled()
	if err != nil {
		return err
	}
	if nativeReceiving && !nativeMail {
		return errors.New("KYPOST_NATIVE_RECEIVING requires KYPOST_NATIVE_MAIL=true")
	}
	if _, err := config.NativeRestoreReleaseEnabled(); err != nil {
		return err
	}
	aiSortingDisabled, err := config.AISortingDisabled()
	if err != nil {
		return err
	}
	kycal, err := kycalendar.FromEnv()
	if err != nil {
		return err
	}
	drive, err := kydrive.FromEnv()
	if err != nil {
		return err
	}
	chat, err := suitepair.FromEnv(api.KyMessagePeer)
	if err != nil {
		return err
	}
	pulse := suitepair.FromKyIdentity(api.KyPulsePeer)
	proxies, err := config.LoadTrustedProxies()
	if err != nil {
		return err
	}
	api.SetTrustedProxies(proxies)
	if _, err := config.LoadRateLimitExemptCIDRs(); err != nil {
		return err
	}
	// The quota is native-only configuration: a bad value never stops IMAP.
	if nativeMail {
		quota, err := config.MailboxQuotaBytes()
		if err != nil {
			return err
		}
		api.SetMailboxQuota(quota)
	}
	paths := config.Paths{
		ConfigFile: filepath.Join(config.ConfigDir(), "config.yaml"),
		StateDir:   config.StateDir(),
	}

	tableMode, _, _ := config.Tenancy()
	releaseDocuments, err := startDocuments(context.Background(), config.ConfigDir(), paths.StateDir, config.SecretDir(), tableMode)
	if err != nil {
		return err
	}
	defer releaseDocuments()

	// Capture legacy notification prefs before LoadOrInit rewrites
	// config.yaml with the trimmed multi-user schema (which drops the old
	// global mode/keywords fields the migration needs).
	legacyPrefs, legacyPrefsOK := config.LoadLegacyNotificationPrefs(paths.ConfigFile)

	cfg, err := config.LoadOrInit(paths.ConfigFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Timezone == "" {
		cfg.Timezone = "America/New_York"
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", cfg.Timezone, err)
	}

	// Auto-populate label allowlist from TUNING.md when the config has none.
	if len(cfg.Labels.Allowlist) == 0 {
		if labels := classifier.ParseAllowedLabels(classifier.LoadTuningText()); len(labels) > 0 {
			cfg.Labels.Allowlist = labels
		}
	}

	logger, err := logging.New()
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}

	configDir := config.ConfigDir()
	usersStore, store, err := openStores(logger, configDir, paths.StateDir, legacyPrefs, legacyPrefsOK)
	if err != nil {
		return err
	}
	// CheckEnv refused multi, so this only refuses a multi install started single (K1).
	tenancyMode, staffOrg, _ := config.Tenancy()
	if err := tenancy.Check(context.Background(), store, paths.StateDir, tenancyMode, staffOrg, func() (bool, error) {
		all, err := usersStore.List()
		if err != nil {
			return false, err
		}
		return tenancy.Empty(len(all), configDir, paths.StateDir)
	}); err != nil {
		return err
	}
	if !dbbackend.Postgres() {
		// Deferred after the instance lock, so it runs first: the log is folded in while this
		// process still holds the lock and after every worker has returned.
		defer checkpointSQLite(logger, paths.StateDir, configDir)
	}

	clearAllMFAIfRequested(logger, usersStore, paths.StateDir)

	healthSvc := health.NewService()
	healthSvc.MarkHealthy()

	// wkdStore is the instance-level WKD domain-claim store. In "all" mode
	// the api server and the poller run as goroutines in one binary and
	// share this instance; under supervisord they are two separate
	// processes (`--mode server` and `--mode daemon`) and each builds its
	// own. Both are correct: wkdpublish.Store serializes every
	// read-modify-write with an inter-process file lock, not just a mutex.
	wkdStore, err := wkdpublish.New(paths.StateDir)
	if err != nil {
		return fmt.Errorf("create wkd publish store: %w", err)
	}

	deps := runDeps{
		nativeMail:        nativeMail,
		nativeReceiving:   nativeReceiving,
		aiSortingDisabled: aiSortingDisabled,
		kycal:             kycal,
		kydrive:           drive,
		kymessage:         chat,
		kypulse:           pulse,
		cfg:               cfg,
		configPath:        paths.ConfigFile,
		configDir:         configDir,
		stateDir:          paths.StateDir,
		logger:            logger,
		store:             store,
		users:             usersStore,
		health:            healthSvc,
		wkdStore:          wkdStore,
	}

	if dbbackend.Postgres() {
		// Replicas refuse an event ID another replica already accepted.
		kycal.ShareReplay(store)
		chat.ShareReplay(store)
		pulse.ShareReplay(store)
	}

	// Signal handling is installed exactly once, here at the real entrypoint, and
	// the run functions below observe it only as a context. os/signal registration
	// is process-global, so doing it inside runServer/runAll left a window between
	// process start and signal.Notify in which SIGTERM kept its default disposition
	// and killed the process outright. Passing a context instead of a channel also
	// lets tests drive shutdown without signalling the whole process.
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	go kycal.Run(ctx, kyIdentityDirectory(configDir), logger)
	if *mode != "daemon" {
		dir := kyIdentityDirectory(configDir)
		go drive.Run(ctx, func() kydrive.Directory { return kydrive.Directory(dir()) }, logger)
		go chat.Run(ctx, func() suitepair.Directory { return suitepair.Directory(dir()) }, logger)
		go pulse.Run(ctx, func() suitepair.Directory { return suitepair.Directory(dir()) }, logger)
	}

	if nativeMail {
		// Here, not for the receiving commands above: they run under ulimit -n 256.
		mailbox.HoldAdmissionHandles(mailbox.MaxHeldAdmissions())
	}
	switch *mode {
	case "daemon":
		return runDaemon(ctx, deps)
	case "server":
		return runServer(ctx, deps)
	case "all":
		return runAll(ctx, deps)
	default:
		return errors.New("invalid mode; expected daemon, server, all, or bootstrap-admin")
	}
}

type runDeps struct {
	nativeMail        bool
	nativeReceiving   bool
	aiSortingDisabled bool
	kycal             *kycalendar.Client
	kydrive           *kydrive.Client
	kymessage         *suitepair.Client
	kypulse           *suitepair.Client
	cfg               config.Config
	configPath        string
	configDir         string
	stateDir          string
	logger            *logging.Logger
	store             *state.Store
	users             *users.Store
	health            *health.Service
	wkdStore          *wkdpublish.Store
}

// defaultEmbedModelDir is where the Dockerfile installs potion-base-8M,
// root-owned and read-only.
const defaultEmbedModelDir = "/opt/kypost/models/potion-base-8M"

// configureSorter turns on the hybrid engine (embedding sorter first, LLM for
// whatever it is unsure of) unless CLASSIFIER_ENGINE=llm.
//
// A value the operator mistyped refuses to start: silently running the other
// engine is the permissive default CONTRIBUTING rules out. A missing or corrupt
// model directory does NOT refuse to start — the LLM still sorts everything,
// and a mail server should not go down over an optional engine — but it is
// logged as an error, never silently.
func configureSorter(log *logging.Logger, poller *processor.Poller) error {
	switch engine := strings.ToLower(strings.TrimSpace(os.Getenv("CLASSIFIER_ENGINE"))); engine {
	case "", "hybrid":
	case "llm":
		log.Info("embedding sorter disabled by CLASSIFIER_ENGINE", "engine", "llm")
		return nil
	default:
		return fmt.Errorf("CLASSIFIER_ENGINE=%q is not valid: use hybrid (the default) or llm", engine)
	}
	minConf := processor.DefaultEmbedMinConfidence
	if v := strings.TrimSpace(os.Getenv("EMBED_MIN_CONFIDENCE")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("EMBED_MIN_CONFIDENCE=%q is not valid: use a number from 0 to 1", v)
		}
		minConf = f
	}
	dir := strings.TrimSpace(os.Getenv("EMBED_MODEL_DIR"))
	if dir == "" {
		dir = defaultEmbedModelDir
	}
	m, err := sorter.Load(dir)
	if err != nil {
		log.Error("embedding sorter unavailable; every message goes to the LLM", "path", dir, "error", err.Error())
		return nil
	}
	poller.SetSorter(m, minConf)
	log.Info("embedding sorter enabled", "engine", "hybrid", "model", sorter.ModelID,
		"confidence", strconv.FormatFloat(minConf, 'f', 2, 64))
	return nil
}

func runDaemon(ctx context.Context, d runDeps) error {
	_, release, err := startReplica(ctx, d, "daemon")
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped before it started
		}
		return err
	}
	defer release()
	ctx, restart := context.WithCancelCause(ctx)
	defer restart(nil)
	go monitorHealth(ctx, d.logger, d.health, healthCheckInterval, restart)
	// A scheduled backup outlives the daemon role: it holds its own run lock, so the role is
	// released first and a standby takes over while this process drains it.
	var backups sync.WaitGroup
	defer backups.Wait()
	// Multi mode's receiving import runs on every daemon, not only the elected one (RX R8).
	importDone, err := startLeasedImport(ctx, d)
	if err != nil {
		return err
	}
	defer func() { restart(nil); <-importDone }()
	waitMail := elect(ctx, d, worker.RoleMailRuntime, func(owned context.Context) { runMailRuntime(owned, d) })
	waitDaemon := elect(ctx, d, worker.RoleDaemon, func(owned context.Context) {
		if err := runDaemonRole(owned, d, &backups); err != nil {
			restart(err)
		}
	})
	<-ctx.Done()
	waitDaemon()
	waitMail()
	return exitCause(ctx)
}

// runDaemonRole is the daemon role, run only by its elected owner: receiving import,
// Cloudflare pickup, the poller (classification, out-of-office, snoozes, native push,
// phishing, heartbeat, WKD recheck, retention cleanup), the classifier warmup and the backup
// schedule. Standby daemons idle until they win the role.
func runDaemonRole(owned context.Context, d runDeps, backups *sync.WaitGroup) error {
	roleCtx, cancel := context.WithCancel(owned)
	defer cancel()
	receivingDone, err := startReceivingImport(roleCtx, d)
	if err != nil {
		return err
	}
	defer func() { cancel(); <-receivingDone }()
	cloudflareDone, err := startCloudflareReceiving(roleCtx, d)
	if err != nil {
		return err
	}
	defer func() { cancel(); <-cloudflareDone }()
	classifierClient := newClassifierClient(d.cfg)
	poller, err := processor.New(d.cfg, d.logger, d.store, d.users, d.stateDir, d.configDir, d.health, classifierClient, d.wkdStore)
	if err != nil {
		return err
	}
	poller.SetConfigPath(d.configPath)
	poller.SetKyCalendar(d.kycal)
	if d.nativeMail {
		poller.EnableNativeMail()
	}
	warmupDone, err := startClassifier(roleCtx, d, classifierClient, poller)
	if err != nil {
		return err
	}
	backupDone, err := startBackupLoop(roleCtx, d, backups)
	if err != nil {
		cancel()
		awaitWarmup(d.logger, warmupDone)
		return err
	}
	defer func() { cancel(); <-backupDone }()
	poller.Start()
	d.logger.Info("poller goroutine started")
	<-owned.Done()
	poller.Stop()
	awaitWarmup(d.logger, warmupDone)
	// Wait for the poller's own goroutines (the tick loop, plus the eager
	// recheckWKDDomains and cleanupAllUsers runs) to finish. Stop only
	// cancels; without this the process could exit, or a test could remove
	// its state directory, while those were still writing to it.
	poller.Wait()
	return nil
}

// exitCause is the error a run ends with: the health monitor's restart, or a role that could
// not start; nil for an ordinary stop.
func exitCause(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// startBackgroundSweepers launches every periodic reclaim the API process needs.
//
// One list, called from both runServer and runAll. As a dozen duplicated
// `go srv.StartXSweeper(ctx)` lines, one added to one list and not the other
// is a leak that only appears in the run mode nobody is testing when they add it.
//
// The memory sweeps run in every process. The durable ones run only on the owner of the
// api-sweepers role, so replicas never sweep, provision or send scheduled mail twice at
// once; the expired shared-row cleanup joins them on PostgreSQL.
//
// The returned wait blocks until every sweeper has returned after ctx is
// canceled, so shutdown does not cut off work already under way.
func startBackgroundSweepers(ctx context.Context, srv *api.Server, d runDeps) (wait func()) {
	local := runSweepers(ctx, d.logger, sweeperDrain, []func(context.Context){
		srv.StartCooldownSweeper,
		srv.StartSessionSweeper,
		srv.StartMFAChallengeSweeper,
		srv.StartPoWSweeper,
		srv.StartUserStoreSweeper,
		srv.StartConfigFollower,
	}...)
	elected := elect(ctx, d, worker.RoleAPISweepers, func(owned context.Context) {
		runSweepers(owned, d.logger, sweeperDrain, []func(context.Context){
			srv.StartNativeProvisioning,
			srv.StartPickupSweeper,
			srv.StartContactPhotoSweeper,
			srv.StartContactsTombstoneSweeper,
			srv.StartEnvelopeSweeper,
			srv.StartVersionMonitor,
			srv.StartScheduledSendSweeper,
			srv.StartAgentTokenSweeper,
			srv.StartOrgPoller,
			func(ctx context.Context) {
				cleanCoordination(ctx, d.store, func(err error) {
					d.logger.Error("expired shared rows not cleaned", "error", err.Error())
				})
			},
		}...)()
	})
	return func() { local(); elected() }
}

// sweeperDrain bounds how long shutdown waits for sweepers; a scheduled send
// already claimed gets its own deadline, well inside this.
const sweeperDrain = 5 * time.Minute

func runSweepers(ctx context.Context, logger *logging.Logger, drain time.Duration, sweeps ...func(context.Context)) (wait func()) {
	var wg sync.WaitGroup
	for _, sweep := range sweeps {
		wg.Go(func() { sweep(ctx) })
	}
	return func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(drain):
			logger.Error("background sweepers still running at shutdown", "limit", drain.String())
		}
	}
}

func runServer(ctx context.Context, d runDeps) error {
	instance, release, err := startReplica(ctx, d, "api")
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped before it started
		}
		return err
	}
	defer release()
	srv := newAPIServer(d, nil)
	if instance != "" {
		srv.SetReplicaInstance(instance, d.store.InstanceLive)
	}
	attachClassifier(srv, d, newClassifierClient(d.cfg))

	// Prepare constructs the *http.Server synchronously, before the Serve
	// goroutine below is even launched, so a stop signal arriving essentially
	// immediately still has a real server for Shutdown to act on (see
	// api.Server.Prepare's doc comment for the race this avoids).
	srv.Prepare()

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	waitMail := elect(workCtx, d, worker.RoleMailRuntime, func(owned context.Context) { runMailRuntime(owned, d) })
	waitSweepers := startBackgroundSweepers(workCtx, srv, d)
	defer func() { cancelWork(); waitSweepers(); waitMail() }()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve()
	}()

	select {
	case <-ctx.Done():
		gracefulStop(srv, cancelWork, d.logger)
		if err := <-serveErr; err != nil {
			d.logger.Error("api server serve error", "error", err.Error())
		}
		return nil
	case err := <-serveErr:
		return err
	}
}

// newAPIServer builds the api server for d; on PostgreSQL its security state is shared with
// every replica through the global state store.
func newAPIServer(d runDeps, onConfigUpdated func(config.Config)) *api.Server {
	srv := api.NewServer(d.cfg, d.logger, d.health, d.users, onConfigUpdated, d.wkdStore)
	srv.SetKyCalendar(d.kycal)
	srv.SetKyDrive(d.kydrive)
	srv.SetKyMessage(d.kymessage)
	srv.SetKyPulse(d.kypulse)
	if d.nativeMail {
		srv.EnableNativeMail()
	}
	if dbbackend.Postgres() {
		srv.UseSharedState(d.store)
	}
	return srv
}

// readinessDrainDelay is how long a PostgreSQL replica keeps serving after /readyz turns 503,
// so the proxy's two failed checks a second apart take it out of rotation before the listener
// closes. One SQLite container has no proxy to wait for.
var readinessDrainDelay = 5 * time.Second

// gracefulStop is the api's SIGTERM sequence: turn /readyz to 503 and refuse new backup
// work; on PostgreSQL keep serving for readinessDrainDelay; then stop the sweepers and
// elections, drain ordinary requests for shutdownTimeout, cancel imports and drain detached
// backups (Shutdown). The caller then joins the sweepers and the outbound workers.
func gracefulStop(srv *api.Server, cancelWork context.CancelFunc, logger *logging.Logger) {
	srv.StartDraining()
	if dbbackend.Postgres() {
		time.Sleep(readinessDrainDelay)
	}
	cancelWork()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("api server shutdown error", "error", err.Error())
	}
}

// shutdownTimeout bounds ordinary HTTP draining and classifier warmup cleanup.
// Detached backup handlers are drained separately, including their audit writes.
const shutdownTimeout = 20 * time.Second

func runAll(ctx context.Context, d runDeps) error {
	if dbbackend.Postgres() {
		return errors.New("--mode all is the single-process mode for one SQLite container; on PostgreSQL run --mode server and --mode daemon, as the image's supervisord does")
	}
	ctx, restart := context.WithCancelCause(ctx)
	defer restart(nil)
	go monitorHealth(ctx, d.logger, d.health, healthCheckInterval, restart)
	outboundCtx, cancelOutbound := context.WithCancel(ctx)
	outboundDone := startNativeOutbound(outboundCtx, d)
	dkimDone := startDKIMRotation(outboundCtx, d)
	defer func() { cancelOutbound(); <-outboundDone; <-dkimDone }()
	receivingCtx, cancelReceiving := context.WithCancel(ctx)
	receivingDone, err := startReceivingImport(receivingCtx, d)
	if err != nil {
		cancelReceiving()
		return err
	}
	defer func() { cancelReceiving(); <-receivingDone }()
	cloudflareDone, err := startCloudflareReceiving(receivingCtx, d)
	if err != nil {
		return err
	}
	defer func() { cancelReceiving(); <-cloudflareDone }()
	// Restore the sticky AI-credits flag onto the health status so a restart
	// keeps surfacing it until a successful classify clears it.
	if exhausted, at := d.store.AICreditsExhausted(); exhausted {
		d.health.SetAICreditsExhausted(at)
	}
	classifierClient := newClassifierClient(d.cfg)
	poller, err := processor.New(d.cfg, d.logger, d.store, d.users, d.stateDir, d.configDir, d.health, classifierClient, d.wkdStore)
	if err != nil {
		return err
	}
	poller.SetConfigPath(d.configPath)
	srv := newAPIServer(d, poller.UpdateConfig)
	poller.SetKyCalendar(d.kycal)
	if d.nativeMail {
		poller.EnableNativeMail()
	}
	srv.SetPoller(poller)
	attachClassifier(srv, d, classifierClient)
	warmupDone, err := startClassifier(ctx, d, classifierClient, poller)
	if err != nil {
		return err
	}

	// Prepare constructs the *http.Server synchronously, before the Serve
	// goroutine below is launched, so a stop signal arriving essentially
	// immediately still has a real server for Shutdown to act on (see
	// api.Server.Prepare's doc comment for the race this avoids).
	srv.Prepare()

	sweeperCtx, cancelSweepers := context.WithCancel(ctx)
	defer cancelSweepers()

	var backups sync.WaitGroup
	defer backups.Wait()
	backupDone, err := startBackupLoop(ctx, d, &backups)
	if err != nil {
		return err
	}
	defer func() { <-backupDone }()
	poller.Start()
	d.logger.Info("poller goroutine started")
	waitSweepers := startBackgroundSweepers(sweeperCtx, srv, d)
	defer func() { cancelSweepers(); waitSweepers() }()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(); err != nil {
			d.logger.Error("api server stopped", "error", err.Error())
		}
	}()

	<-ctx.Done()
	// Stop the poller before draining HTTP: Stop cancels every tick, including
	// one an in-flight "poll now" request is running, so that request returns
	// instead of holding Shutdown for its whole window.
	cancelSweepers()
	poller.Stop()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		d.logger.Error("api server shutdown error", "error", err.Error())
	}
	// Wait for the Serve goroutine to actually return, exactly as runServer
	// does. Without this, runAll reported a completed shutdown while Serve was
	// still unwinding and still writing — the caller would then close the
	// logger, and the process would exit, mid-write. It surfaced as a flaky
	// "TempDir RemoveAll cleanup: directory not empty" in the shutdown test:
	// the test's temp dir was still being written after runAll had returned.
	<-serveDone
	awaitWarmup(d.logger, warmupDone)
	// Wait for the poller's own goroutines (the tick loop, plus the eager
	// recheckWKDDomains and cleanupAllUsers runs) to finish. Stop only
	// cancels; without this the process could exit, or a test could remove
	// its state directory, while those were still writing to it.
	poller.Wait()
	return healthRestart(ctx)
}

// mfaClearAllMarkerFile is the break-glass one-shot marker: once MFA_CLEAR_ALL
// has successfully cleared every user's MFA, this file's presence in stateDir
// self-disarms the env var so leaving it set after the fact doesn't silently
// wipe out MFA that users re-enroll on every subsequent restart.
const mfaClearAllMarkerFile = "mfa-clear-all.done"

// mfaClearAllProgressFile persists, per user ID, which users MFA_CLEAR_ALL has
// already cleared during a campaign that has not yet fully succeeded and written
// mfaClearAllMarkerFile.
//
// Without it, a boot that clears users A and B but fails on C writes no marker,
// so the next boot reruns the ENTIRE user list. If A or B re-enrolled MFA in the
// meantime — exactly what the break-glass procedure expects an admin to do — the
// retry silently wipes it out again, and forever if C's failure is permanent.
// Tracking per-user completion means a retry only touches users still
// outstanding.
const mfaClearAllProgressFile = "mfa-clear-all.progress"

// mfaClearAllProgress is the on-disk schema for mfaClearAllProgressFile.
type mfaClearAllProgress struct {
	Cleared []string `json:"cleared"`
}

// loadMFAClearAllCleared reads the set of user IDs already successfully
// cleared by a previous boot's MFA_CLEAR_ALL pass. A missing file (no prior
// partial attempt) is not an error and yields an empty set.
func loadMFAClearAllCleared(path string) (map[string]struct{}, error) {
	data, err := vol.OpenDocument(path).Read()
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	var progress mfaClearAllProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return nil, err
	}
	cleared := make(map[string]struct{}, len(progress.Cleared))
	for _, id := range progress.Cleared {
		cleared[id] = struct{}{}
	}
	return cleared, nil
}

// saveMFAClearAllCleared atomically persists the set of user IDs cleared so
// far, so a subsequent boot (should this one fail partway) knows which users
// are already done and must not be touched again.
func saveMFAClearAllCleared(path string, cleared map[string]struct{}) error {
	ids := make([]string, 0, len(cleared))
	for id := range cleared {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data, err := json.Marshal(mfaClearAllProgress{Cleared: ids})
	if err != nil {
		return err
	}
	return vol.OpenDocument(path).Replace(data)
}

// clearAllMFAIfRequested is a break-glass recovery path for self-hosters locked
// out by MFA with no other admin able to reach the Manage Users page. Setting
// MFA_CLEAR_ALL wipes TOTP/recovery codes/push-MFA for every user, but only
// once: a successful clear writes a marker file in stateDir that permanently
// disarms this path, so an operator who forgets to unset the env var does not
// keep re-clearing MFA on every future boot.
//
// If the clear fails partway, the marker is deliberately not written so the next
// boot retries — but only for users not yet recorded in mfaClearAllProgressFile.
func clearAllMFAIfRequested(logger *logging.Logger, usersStore *users.Store, stateDir string) {
	// Run's config.CheckEnv has already refused a malformed value.
	if enabled, _ := config.EnvBool("MFA_CLEAR_ALL"); !enabled {
		return
	}

	markerPath := filepath.Join(stateDir, mfaClearAllMarkerFile)
	if exists, err := vol.Exists(markerPath); err == nil && exists {
		return
	} else if err != nil {
		logger.Error("MFA_CLEAR_ALL: failed to check completion marker; skipping clear this boot", "error", err.Error())
		return
	}

	progressPath := filepath.Join(stateDir, mfaClearAllProgressFile)
	cleared, err := loadMFAClearAllCleared(progressPath)
	if err != nil {
		logger.Error("MFA_CLEAR_ALL: failed to read per-user clear progress; skipping clear this boot to avoid re-clearing already-handled users", "error", err.Error())
		return
	}

	all, err := usersStore.List()
	if err != nil {
		logger.Error("MFA_CLEAR_ALL: failed to list users", "error", err.Error())
		return
	}

	clearedThisBoot := 0
	failed := false
	for _, u := range all {
		if _, alreadyCleared := cleared[u.ID]; alreadyCleared {
			// Already successfully cleared by a previous boot of this
			// campaign. Skip unconditionally — even if they now show
			// TOTPEnabled/PushMFAEnabled again because they re-enrolled —
			// so an unrelated user's outstanding failure elsewhere doesn't
			// cause a retry to wipe out their fresh MFA a second time.
			continue
		}
		if !u.TOTPEnabled && !u.PushMFAEnabled {
			continue
		}
		if _, err := usersStore.DisableTOTP(u.ID); err != nil {
			logger.Error("MFA_CLEAR_ALL: failed to clear user", "user_id", u.ID, "error", err.Error())
			failed = true
			continue
		}
		cleared[u.ID] = struct{}{}
		clearedThisBoot++
	}

	if err := saveMFAClearAllCleared(progressPath, cleared); err != nil {
		// The clears that already happened are real and won't be undone,
		// but without a persisted record of them a later boot can't tell
		// they're done, so force a retry path rather than risk declaring
		// this campaign complete based on an unpersisted set.
		logger.Error("MFA_CLEAR_ALL: failed to persist per-user clear progress; will retry on next boot", "error", err.Error())
		failed = true
	}

	if failed {
		logger.Error("MFA_CLEAR_ALL: cleared two-factor auth for some users this boot, but at least one user failed or progress could not be saved; outstanding users will be retried on next boot", "users_cleared_this_boot", strconv.Itoa(clearedThisBoot))
		return
	}

	logger.Error("MFA_CLEAR_ALL is set: cleared two-factor auth for all users", "users_cleared_this_boot", strconv.Itoa(clearedThisBoot))

	if err := vol.OpenDocument(markerPath).Replace([]byte(time.Now().UTC().Format(time.RFC3339) + "\n")); err != nil {
		logger.Error("MFA_CLEAR_ALL: failed to write completion marker; will retry clearing MFA on next boot", "error", err.Error())
		return
	}
	logger.Error("MFA_CLEAR_ALL: cleared two-factor auth for all users and wrote a completion marker; this env var can now be left set safely, it will not clear MFA again")
}

// errUnhealthy ends a run that stayed unhealthy past UNHEALTHY_RESTART_SECONDS;
// the non-zero exit lets supervisord restart the process.
var errUnhealthy = errors.New("unhealthy threshold exceeded; restarting")

// healthCheckInterval is a var so tests can shorten it; read it before `go`.
var healthCheckInterval = 15 * time.Second

// monitorHealth calls restart once health has been bad past the threshold.
// Restart cancels the run's context, so shutdown drains exactly as on SIGTERM.
func monitorHealth(ctx context.Context, logger *logging.Logger, healthSvc *health.Service, interval time.Duration, restart context.CancelCauseFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	threshold, _ := config.UnhealthyRestartSeconds.Get() // validated by Run's config.CheckEnv
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st := healthSvc.GetStatus()
		if st.Healthy || st.UnhealthyFor < int64(threshold) {
			continue
		}
		logger.Error("unhealthy threshold exceeded; shutting down so supervisord restarts this process",
			"unhealthy_for_seconds", strconv.FormatInt(st.UnhealthyFor, 10))
		restart(errUnhealthy)
		return
	}
}

// healthRestart returns errUnhealthy when monitorHealth ended the run.
func healthRestart(ctx context.Context) error {
	if err := context.Cause(ctx); errors.Is(err, errUnhealthy) {
		return err
	}
	return nil
}

// newClassifierClient builds the one shared LLM client from the OLLAMA_* env
// vars. The config-file endpoint ("Remote LLM") was removed: it let an operator
// point classification at an arbitrary host from the web UI, which meant email
// content and an API key could be redirected by anyone who reached that form.
func newClassifierClient(cfg config.Config) *classifier.HTTPClient {
	baseURL := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("CLASSIFIER_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}

	apiKey := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))

	classifyPath := strings.TrimSpace(os.Getenv("OLLAMA_GENERATE_PATH"))
	if classifyPath == "" {
		classifyPath = "/api/generate"
	}

	// Reported, not enforced. The only way to arrive here with a failing base
	// URL is now an env var the operator set directly, which is not a reason to
	// refuse to start a mail server. What it is a reason for is saying so out
	// loud once per boot, because the failure mode this catches (email and the
	// API key crossing the public internet in the clear) has no symptom
	// otherwise: classification keeps working exactly as before.
	if err := classifier.ValidateBaseURL(baseURL); err != nil {
		slog.Error("classifier endpoint fails the transport policy; email content is being sent to it anyway",
			"error", err.Error())
	}

	// The default tuning text only backstops callers that pass no per-call
	// tuning (e.g. users who have not customized their prompt yet).
	tuning := classifier.LoadTuningText()
	return classifier.NewHTTPClient(baseURL, apiKey, classifyPath, tuning, 3*time.Minute)
}

// warmupClassifierOnStartup kicks off the model warmup and the post-warmup
// unread sweep in the background, returning a channel closed once that work has
// finished unwinding.
//
// The context is the caller's shutdown context, not context.Background(). As
// Background with a bare 5-minute timeout, the warmup goroutine kept issuing
// IMAP calls and writing per-user state for up to five minutes after the process
// had been told to shut down — and TriggerUnreadSweep resets every active user's
// checkpoint before it re-scans, so starting that during shutdown risks tearing
// the write.
func warmupClassifierOnStartup(ctx context.Context, logger *logging.Logger, client *classifier.HTTPClient, poller *processor.Poller) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		logger.Info("classifier startup warmup requested")
		if err := client.Warmup(ctx); err != nil {
			logger.Error("classifier startup warmup failed", "error", err.Error())
			return
		}
		logger.Info("classifier startup warmup completed")
		// Re-check before the sweep: Warmup can return nil on a context that
		// was cancelled moments later, and the sweep is the half that writes.
		if ctx.Err() != nil {
			logger.Info("skipping post-warmup unread sweep; shutting down")
			return
		}
		logger.Info("processing unread unlabeled mail after startup warmup")
		poller.TriggerUnreadSweep()
	}()
	return done
}

// startClassifier configures the embedding sorter and starts the Ollama warmup,
// unless KYPOST_DISABLE_AI_SORTING is set: then the poller labels by filter
// rules and the fallback label only, and nothing ever calls Ollama. The
// returned channel is closed when there is no warmup to wait for.
func startClassifier(ctx context.Context, d runDeps, client *classifier.HTTPClient, poller *processor.Poller) (<-chan struct{}, error) {
	if d.aiSortingDisabled {
		d.logger.Info("AI sorting disabled by KYPOST_DISABLE_AI_SORTING; new mail gets the fallback label")
		poller.DisableAISorting()
		done := make(chan struct{})
		close(done)
		return done, nil
	}
	if err := configureSorter(d.logger, poller); err != nil {
		return nil, err
	}
	return warmupClassifierOnStartup(ctx, d.logger, client, poller), nil
}

// attachClassifier gives the API the classifier for the Ollama version monitor
// and queue stats. With AI sorting disabled it gets none, so the monitor never
// probes Ollama or emails the admin about Ollama updates.
func attachClassifier(srv *api.Server, d runDeps, client *classifier.HTTPClient) {
	if d.aiSortingDisabled {
		srv.DisableAISorting()
		return
	}
	srv.SetClassifier(client)
}

// awaitWarmup waits for the startup warmup goroutine to unwind, bounded so a
// sweep that is already in flight cannot hold shutdown open indefinitely.
func awaitWarmup(logger *logging.Logger, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		logger.Error("startup warmup did not finish within the shutdown timeout; exiting anyway")
	}
}

// kyIdentityDirectory reads KyPost's SSO client for kycalendar.Run, which
// rereads it so a settings change needs no restart. Zero unless SSO is on.
func kyIdentityDirectory(configDir string) func() kycalendar.Directory {
	store := sso.NewStore(configDir)
	return func() kycalendar.Directory {
		d, ok := store.Load().SuiteDirectory()
		if !ok {
			return kycalendar.Directory{}
		}
		return kycalendar.Directory{Issuer: d.Issuer, ClientID: d.ClientID, ClientSecret: d.ClientSecret}
	}
}
