// Package backup is kypost's adapter over ky-primitives/recoveryclient: what
// to seal, which key seals the pairing token, where settings and audit rows
// live. How a capsule is made, delivered or restored is the library's.
package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	kylog "github.com/Busnes-app/ky-primitives/logging"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// AppName is the service_name claimed at pairing and named in every manifest.
// KyRecovery pins it per token and refuses a deposit whose manifest says otherwise.
const AppName = "KyPost"

const tokenLabel = "kypost:setting:kyrecovery_token"

// ErrKeyAlreadyPinned wraps the library's fs.ErrExist from a second pin to a
// different key, so handlers have one name to map to 409.
var ErrKeyAlreadyPinned = errors.New("backup: a different recovery key is already pinned")

// Dirs are the three data roots a restore needs. Logs are not backed up.
type Dirs struct{ Config, State, Secret string }

// settings adapts the install-wide store to the library's Settings.
type settings struct{ st *state.Store }

func (s settings) Get(key string) (string, error) {
	if err := settingsRead(); err != nil {
		return "", err
	}
	v, err := s.st.Setting(key)
	if errors.Is(err, state.ErrNotFound) {
		return "", recoveryclient.ErrNotFound
	}
	return v, err
}
func (s settings) Set(key, val string) error { return s.st.SetSetting(key, val) }
func (s settings) Delete(key string) error   { return s.st.DeleteSetting(key) }

// Service is one process's handle on backups.
type Service struct {
	dirs     Dirs
	cfg      config.BackupConfig
	store    *state.Store
	settings settings
	client   *recoveryclient.Client
	version  string
	table    bool
	logger   *kylog.Logger
	// collectedBulk is the bulk snapshot of the payload being sealed, under the lock.
	collectedBulk *BulkSnapshot
}

// New binds the data roots. Existing keys are loaded at operation time, so a
// fresh API can initialize its TOTP key before the first backup without the
// daemon generating a replacement key for encrypted data.
func New(d Dirs, bc config.BackupConfig, store *state.Store, appVersion string) (*Service, error) {
	cfg, err := kylog.FromEnv()
	if err != nil {
		return nil, err
	}
	cfg.App = "kypost"
	logger, err := kylog.New(cfg)
	if err != nil {
		return nil, err
	}
	for _, root := range []string{d.Config, d.State, d.Secret} {
		if !filepath.IsAbs(root) {
			return nil, errors.New("backup data directories must be absolute")
		}
	}
	table := vol.OpenDocument(filepath.Join(d.Config, "config.yaml")).IsTable()
	if bc.Dir != "" && table {
		return nil, errors.New("table-mode backups refuse KYPOST_BACKUP_DIR; unset it and pair KyRecovery for backup deposits")
	}
	if bc.Dir != "" {
		for _, root := range []string{d.Config, d.Secret, d.State} {
			rel, err := filepath.Rel(root, bc.Dir)
			if err != nil {
				return nil, err
			}
			inside := rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
			if inside && bc.Dir != filepath.Join(d.State, "backups") {
				return nil, errors.New("local backup directory must be outside data roots or exactly STATE_DIR/backups")
			}
		}
		for _, root := range []string{d.Config, d.State, d.Secret} {
			rel, err := filepath.Rel(bc.Dir, root)
			if err != nil {
				return nil, err
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return nil, errors.New("backup directory must not contain a data root")
			}
		}
	}
	if repo := bc.BulkRepository; repo != "" && !strings.HasPrefix(repo, "rest:") {
		for _, other := range []string{d.Config, d.State, d.Secret, bc.Dir, bc.ScratchDir} {
			if other != "" && overlaps(repo, other) {
				return nil, errors.New("KYPOST_BULK_BACKUP_REPOSITORY must be outside every data root, the local backup directory and the scratch directory")
			}
		}
	}
	if dir := bc.ScratchDir; dir != "" {
		for _, other := range []string{d.Config, d.State, d.Secret, bc.Dir} {
			if other != "" && overlaps(dir, other) {
				return nil, errors.New("KYPOST_BACKUP_SCRATCH_DIR must be outside every data root and the local backup directory")
			}
		}
	}

	return &Service{
		dirs: d, cfg: bc, store: store, settings: settings{store}, logger: logger,
		client:  recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: bc.AllowPrivateRecovery}),
		version: appVersion, table: table,
	}, nil
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// overlaps reports whether either path is inside the other, by name or by file
// identity, so symlinks and bind-mounted aliases of a data root count too.
func overlaps(a, b string) bool {
	return within(a, b) || within(b, a) || sameAncestor(a, b) || sameAncestor(b, a)
}

// sameAncestor reports whether p, or a directory above it, is the file root.
func sameAncestor(p, root string) bool {
	ri, err := os.Stat(root)
	if err != nil {
		return false
	}
	for ; ; p = filepath.Dir(p) {
		if pi, err := os.Stat(p); err == nil && os.SameFile(pi, ri) {
			return true
		}
		if p == filepath.Dir(p) {
			return false
		}
	}
}

// AllowPrivate reports the KYPOST_BACKUP_ALLOW_PRIVATE_RECOVERY switch, for the
// pairing audit row and the screen.
func (s *Service) AllowPrivate() bool { return s.cfg.AllowPrivateRecovery }

func (s *Service) runConfig(sealer recoveryclient.Sealer) recoveryclient.RunConfig {
	return recoveryclient.RunConfig{
		DataDir: s.dirs.State, AppName: AppName, AppVersion: s.version,
		BackupDir: s.cfg.Dir, Keep: s.cfg.Keep, Sealer: sealer,
	}
}

// Run seals once and delivers to every configured destination.
// On PostgreSQL it holds backup-run; losing it cancels collection and deposit. Every
// setting it reads comes from one statement.
func (s *Service) Run(ctx context.Context) (recoveryclient.Result, error) {
	return s.run(ctx, false, nil)
}

// ErrNotDue is a scheduled run another process already made: due is read under the locks.
var ErrNotDue = errors.New("backup: not due")

// RunIfDue is the scheduler's Run: it reads the schedule under the locks, so of several
// processes that saw the same due tick, one makes the capsule. started runs once the run is
// due and owned, before anything else (the intent audit).
func (s *Service) RunIfDue(ctx context.Context, started func() error) (recoveryclient.Result, error) {
	return s.run(ctx, true, started)
}

func (s *Service) run(ctx context.Context, dueOnly bool, started func() error) (recoveryclient.Result, error) {
	var res recoveryclient.Result
	err := s.withOperation(ctx, func(ctx context.Context) error {
		return s.withRunLock(ctx, func(ctx context.Context) error {
			if err := s.SyncRecoveryKey(); err != nil {
				return err
			}
			f, err := s.snapshot()
			if err != nil {
				return err
			}
			if dueOnly {
				next, on, err := recoveryclient.NextRun(s.cfg.DepositInterval, f)
				if err != nil {
					return err
				}
				if !on || time.Now().Before(next) {
					return ErrNotDue
				}
			}
			if started != nil {
				if err := started(); err != nil {
					return err
				}
			}
			if err := f.Set("backup_last_attempt", time.Now().UTC().Format(time.RFC3339)); err != nil {
				return err
			}
			if _, err := pairingConfigured(f); err != nil {
				return err
			}
			sealer, err := s.loadSealer()
			if err != nil {
				return err
			}
			if _, err = s.loadKey(f); err != nil {
				return err
			}
			res, err = recoveryclient.Run(ctx, s.runConfig(sealer), f, func() (recoveryclient.Payload, error) { return s.collect(ctx) }, s.client)
			if res.LocalPath != "" || res.Receipt != nil {
				s.recordBulk()
			}
			if err == nil && ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			return err
		})
	})
	return res, err
}

// Export seals once for download; nothing is delivered and nothing is stamped.
func (s *Service) Export(ctx context.Context) ([]byte, capsule.Manifest, error) {
	var out []byte
	var manifest capsule.Manifest
	err := s.withOperation(ctx, func(ctx context.Context) error {
		return s.withRunLock(ctx, func(ctx context.Context) error {
			if err := s.SyncRecoveryKey(); err != nil {
				return err
			}
			f, err := s.snapshot()
			if err != nil {
				return err
			}
			key, err := s.loadKey(f)
			if err != nil {
				return err
			}
			p, err := s.collect(ctx)
			if err != nil {
				return err
			}
			if out, manifest, err = recoveryclient.Seal(p, key); err == nil && ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			return err
		})
	})
	if err != nil {
		return nil, capsule.Manifest{}, err
	}
	return out, manifest, nil
}

// Pair claims a pairing code, pins the key write-once and stores the sealed token.
func (s *Service) Pair(ctx context.Context, rawURL, code string) (recoveryclient.RecoveryKey, error) {
	return s.pair(ctx, rawURL, code, false)
}

// PairIfMissing is installer setup: under the ordinary backup fence, retain
// the existing destination and key without spending a new pairing code.
func (s *Service) PairIfMissing(ctx context.Context, rawURL, code string) (recoveryclient.RecoveryKey, error) {
	return s.pair(ctx, rawURL, code, true)
}

func (s *Service) pair(ctx context.Context, rawURL, code string, onlyMissing bool) (recoveryclient.RecoveryKey, error) {
	var key recoveryclient.RecoveryKey
	err := s.withOperation(ctx, func(ctx context.Context) error {
		var err error
		key, err = s.pairHeld(ctx, rawURL, code, onlyMissing)
		return err
	})
	return key, err
}

func (s *Service) pairHeld(ctx context.Context, rawURL, code string, onlyMissing bool) (recoveryclient.RecoveryKey, error) {
	if onlyMissing {
		f, err := s.snapshot()
		if err != nil {
			return recoveryclient.RecoveryKey{}, err
		}
		paired, err := pairingConfigured(f)
		if err != nil {
			return recoveryclient.RecoveryKey{}, err
		}
		if paired {
			existing, err := f.Get("kyrecovery_url")
			if err != nil || existing != rawURL {
				return recoveryclient.RecoveryKey{}, errors.New("existing recovery destination differs; setup will not replace it")
			}
			return s.loadKey(f)
		}
	}
	sealer, err := s.loadSealer()
	if err != nil {
		return recoveryclient.RecoveryKey{}, err
	}
	if err := recoveryclient.ValidateURL(rawURL, s.cfg.AllowPrivateRecovery); err != nil {
		return recoveryclient.RecoveryKey{}, err
	}
	res, err := s.client.ClaimPairing(ctx, rawURL, code, AppName, AppName)
	if err != nil {
		return recoveryclient.RecoveryKey{}, err
	}
	return res.Key, s.storePin(res.Key, func(st recoveryclient.Settings) error {
		return recoveryclient.StorePairing(st, sealer, rawURL, res.APIToken)
	})
}

// storePin pins k write-once (and runs also in the same transaction), records its sealed
// anchor on PostgreSQL and reads the pin back, all under the recovery-pin lock. The same key
// with the same topology succeeds again; another topology is ErrTopologyPinned.
func (s *Service) storePin(k recoveryclient.RecoveryKey, also func(recoveryclient.Settings) error) error {
	return s.withPinLock(func() error {
		err := s.store.BackupSettingsTransaction(func(st recoveryclient.Settings) error {
			if err := pinTopology(st, k); err != nil {
				return err
			}
			if err := recoveryclient.StoreRecoveryKey(s.dirs.State, st, k); err != nil {
				return pinErr(err)
			}
			if also != nil {
				return also(st)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := s.recordAnchor(k); err != nil {
			return err
		}
		f, err := s.snapshot()
		if err != nil {
			return err
		}
		got, err := s.loadKey(f)
		if err != nil {
			return err
		}
		if got.Public.ID() != k.Public.ID() || got.Threshold != k.Threshold || got.TotalShares != k.TotalShares {
			return ErrKeyAlreadyPinned
		}
		return nil
	})
}

// PinKey is the offline path to the same write-once pin.
func (s *Service) PinKey(publicKeyB64 string, k, n int) error {
	key, err := recoveryclient.ParsePinRequest(publicKeyB64, k, n)
	if err != nil {
		return err
	}
	return s.withOperation(context.Background(), func(context.Context) error { return s.storePin(key, nil) })
}

func pinErr(err error) error {
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %v", ErrKeyAlreadyPinned, err)
	}
	return err
}

// Unpair removes the URL and sealed token rows and nothing else.
func (s *Service) Unpair() error {
	return s.withOperation(context.Background(), func(context.Context) error {
		return s.withPinLock(func() error { return s.store.BackupSettingsTransaction(recoveryclient.ClearPairing) })
	})
}

// SetIntervalSeconds stores the admin's schedule; the library bounds it.
func (s *Service) SetIntervalSeconds(sec int64) error {
	return s.withOperation(context.Background(), func(context.Context) error { return recoveryclient.SetInterval(s.settings, sec) })
}

// NextRun is what the daemon loop polls.
func (s *Service) NextRun() (time.Time, bool, error) {
	return recoveryclient.NextRun(s.cfg.DepositInterval, s.settings)
}

// Audit records one row. A caller that cannot audit refuses (see api).
func (s *Service) Audit(action, actor, target, outcome string, details map[string]any) error {
	if err := s.store.RecordBackupAudit(action, actor, target, outcome, details); err != nil {
		return err
	}
	s.logger.Security(context.Background(), backupAuditEvent, kylog.Action(action), kylog.ActorID(actor), kylog.TargetID(target), kylog.Outcome(outcome))
	return nil
}

// Status is what the screen renders.
type Status struct {
	KeyID         string                     `json:"keyId,omitempty"`
	Threshold     int                        `json:"threshold,omitempty"`
	TotalShares   int                        `json:"totalShares,omitempty"`
	KeyProblem    string                     `json:"keyProblem,omitempty"`
	Paired        bool                       `json:"paired"`
	KyRecoveryURL string                     `json:"kyrecoveryUrl,omitempty"`
	LastReceipt   *recoveryclient.Receipt    `json:"lastReceipt,omitempty"`
	LocalDir      string                     `json:"localDir,omitempty"`
	LocalCopies   []recoveryclient.LocalCopy `json:"localCopies"`
	IntervalSec   int64                      `json:"intervalSec"`
	NextRun       string                     `json:"nextRun,omitempty"`
	AllowPrivate  bool                       `json:"allowPrivateRecovery"`
	Excluded      string                     `json:"excluded"`
	BulkRepo      string                     `json:"bulkRepository,omitempty"`
	LastBulk      *BulkSnapshot              `json:"lastBulk,omitempty"`
	Recent        []state.BackupAudit        `json:"recent"`
}

func (s *Service) Status() (Status, error) {
	var status Status
	err := s.withOperation(context.Background(), func(context.Context) error {
		var err error
		status, err = s.InspectStatus()
		return err
	})
	return status, err
}

// InspectStatus is an advisory snapshot: no operation lock is created and no pairing, pin or audit is written.
func (s *Service) InspectStatus() (Status, error) {
	var err error
	st := Status{LocalDir: s.cfg.Dir, AllowPrivate: s.cfg.AllowPrivateRecovery, Excluded: ErrMailExcluded, LocalCopies: []recoveryclient.LocalCopy{}, BulkRepo: redactRepository(s.cfg.BulkRepository)}
	if st.LastBulk, err = s.lastBulk(); err != nil {
		return st, err
	}
	f, err := s.snapshot()
	if err != nil {
		return st, err
	}
	key, err := s.loadKey(f)
	switch {
	case err == nil:
		st.KeyID, st.Threshold, st.TotalShares = key.Public.ID(), key.Threshold, key.TotalShares
	case errors.Is(err, recoveryclient.ErrNotPaired):
	case errors.Is(err, recoveryclient.ErrKeyMismatch), errors.Is(err, recoveryclient.ErrKeyPinMissing):
		st.KeyProblem = "recovery.pub does not match the pinned key ID; backups are refused until it is restored"
	default:
		return st, err
	}
	if st.KeyProblem == "" && dbbackend.Postgres() {
		if err := s.checkAnchor(f); err != nil {
			st.KeyProblem = err.Error()
		}
	}
	paired, err := pairingConfigured(f)
	if err != nil {
		return st, err
	}
	st.Paired = paired
	if paired {
		st.KyRecoveryURL, err = f.Get("kyrecovery_url")
		if err != nil {
			return st, err
		}
	}

	if r, ok, err := recoveryclient.LastDeposit(f); err != nil {
		return st, err
	} else if ok {
		st.LastReceipt = &r
	}
	if s.cfg.Dir != "" {
		copies, err := recoveryclient.ListLocalCopies(s.cfg.Dir, AppName)
		if err != nil {
			return st, err
		}
		// ListLocalCopies returns nil for an empty or missing directory; keep JSON `[]`.
		st.LocalCopies = append(st.LocalCopies, copies...)
	}
	iv, err := recoveryclient.Interval(s.cfg.DepositInterval, f)
	if err != nil {
		return st, err
	}
	st.IntervalSec = int64(iv / time.Second)
	if next, on, err := recoveryclient.NextRun(s.cfg.DepositInterval, f); err != nil {
		return st, err
	} else if on {
		st.NextRun = next.UTC().Format(time.RFC3339)
	}
	recent, err := s.store.RecentBackupAudit(20)
	if err != nil {
		return st, err
	}
	st.Recent = recent
	return st, nil
}

// withOperation keeps the original file lock in ordinary mode. Table-mode
// operations share one probed fence across hosts, acquired within the fence wait
// bound. The callback's context ends if ownership is lost.
func (s *Service) withOperation(ctx context.Context, work func(context.Context) error) error {
	if !s.table {
		release, err := s.lock()
		if err != nil {
			return err
		}
		defer release()
		return work(ctx)
	}
	err := s.store.WithFences(ctx, []state.Fence{{Name: state.FenceBackupOperation}}, func(owned context.Context) error {
		err := work(owned)
		if err == nil && owned.Err() != nil {
			return context.Cause(owned)
		}
		return err
	})
	if errors.Is(err, state.ErrFenceWait) {
		return recoveryclient.ErrInProgress
	}
	return err
}

// Ordinary-mode API and daemon operations retain their nonblocking flock.
// A busy caller retries rather than waiting behind a long local-copy operation.
func (s *Service) lock() (func(), error) {
	// Read-only: flock needs no write access and the lock file holds no data.
	f, err := os.OpenFile(filepath.Join(s.dirs.State, "backup-operation.lock"), os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, recoveryclient.ErrInProgress
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func (s *Service) loadSealer() (recoveryclient.Sealer, error) {
	key, err := cryptutil.LoadKey(s.secretPath("TOTP_SECRET_KEY_FILE", "totp-secret.key"))
	if err != nil {
		return nil, fmt.Errorf("backup requires the existing TOTP master key: %w", err)
	}
	return recoveryclient.NewAESGCMSealer(key, tokenLabel)
}
func (s *Service) secretPath(env, name string) string {
	if path := os.Getenv(env); path != "" {
		return path
	}
	return filepath.Join(s.dirs.Secret, name)
}
func (s *Service) loadKey(f recoveryclient.Settings) (recoveryclient.RecoveryKey, error) {
	key, err := recoveryclient.LoadRecoveryKey(s.dirs.State, f)
	if errors.Is(err, recoveryclient.ErrNotPaired) {
		_, pinErr := f.Get("kyrecovery_key_id")
		if pinErr == nil {
			return key, recoveryclient.ErrKeyPinMissing
		}
		if !errors.Is(pinErr, recoveryclient.ErrNotFound) {
			return key, pinErr
		}
	}
	return key, err
}

func pairingConfigured(f recoveryclient.Settings) (bool, error) {
	url, ue := f.Get("kyrecovery_url")
	token, te := f.Get("kyrecovery_token_enc")
	if ue != nil && !errors.Is(ue, recoveryclient.ErrNotFound) {
		return false, ue
	}
	if te != nil && !errors.Is(te, recoveryclient.ErrNotFound) {
		return false, te
	}
	if url == "" && token == "" {
		return false, nil
	}
	if url == "" || token == "" {
		return false, errors.New("incomplete KyRecovery pairing; unpair and pair again")
	}
	return true, nil
}

// Configured distinguishes a never-configured instance from a broken pin.
func (s *Service) Configured() (bool, error) {
	f, err := s.snapshot()
	if err != nil {
		return false, err
	}
	for _, key := range []string{"kyrecovery_key_id", "kyrecovery_url", "kyrecovery_token_enc"} {
		if _, ok := f.vals[key]; ok {
			return true, nil
		}
	}
	return false, nil
}

var backupAuditEvent = kylog.DeclareEvent("kypost_backup_action", "backup action", slog.LevelInfo)

func (s *Service) IntervalSeconds() (int64, error) {
	d, err := recoveryclient.Interval(s.cfg.DepositInterval, s.settings)
	return int64(d / time.Second), err
}
func (s *Service) PinnedKeyID() (string, error) {
	f, err := s.snapshot()
	if err != nil {
		return "", err
	}
	key, err := s.loadKey(f)
	if err != nil {
		return "", err
	}
	return key.Public.ID(), nil
}

// InspectPairing verifies retained credentials and topology without a provider call,
// mutation or creating an operation lock. Mutation rechecks under its normal fence.
func (s *Service) InspectPairing() (recoveryclient.Pairing, error) {
	sealer, err := s.loadSealer()
	if err != nil {
		return recoveryclient.Pairing{}, err
	}
	f, err := s.snapshot()
	if err != nil {
		return recoveryclient.Pairing{}, err
	}
	p, err := recoveryclient.LoadPairing(s.dirs.State, f, sealer)
	if err != nil {
		return p, err
	}
	if _, err := recoveryclient.ParsePinRequest(base64.StdEncoding.EncodeToString(p.Key.Public.Bytes()), p.Key.Threshold, p.Key.TotalShares); err != nil {
		return p, err
	}
	if p.Token == "" || recoveryclient.ValidateURL(p.URL, s.cfg.AllowPrivateRecovery) != nil {
		return p, errors.New("retained recovery pairing invalid")
	}
	return p, nil
}
