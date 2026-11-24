package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// Backups on PostgreSQL replicas. Every replica mounts the same STATE_DIR, so the
// backup-operation flock already serialises operations on one host; the database adds a
// backup-run lock whose loss cancels a run, a recovery-pin lock around pin, pairing and
// unpair, and a sealed copy of the pinned public key (the anchor) that rewrites a missing
// recovery.pub and catches one that names another key.

var (
	// ErrSettingsUnreadable is a settings read that failed: unknown, never "not paired".
	ErrSettingsUnreadable = errors.New("backup: the backup settings could not be read")
	// ErrTopologyPinned refuses a re-pin of the pinned key with another custodian topology.
	ErrTopologyPinned = errors.New("backup: this key is already pinned with another custodian topology")
	// ErrPinNotRecorded is a pin whose rows committed but whose sealed record did not.
	ErrPinNotRecorded = errors.New("backup: the key is pinned but its record was not stored; pin it again to store the record")
	// ErrAnchor refuses a run whose recovery.pub or record disagrees with the pin.
	ErrAnchor = errors.New("backup: the recovery key record does not match the pin")
)

const (
	anchorSetting = "kyrecovery_public_key"
	anchorLabel   = "kypost:setting:recovery_public_key"
	pinLockWait   = 10 * time.Second
)

// frozen serves the library's reads from one statement's snapshot of every kyrecovery_ and
// backup_ row, so one operation never pairs a URL with another pairing's token; writes pass
// through and update the snapshot.
type frozen struct {
	base settings
	vals map[string]string
}

// settingsRead runs before every database read of backup settings; tests re-pair or fail
// reads through it.
var settingsRead = func() error { return nil }

func (s *Service) snapshot() (*frozen, error) {
	if err := settingsRead(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSettingsUnreadable, err)
	}
	vals, err := s.store.SettingsWithPrefixes("kyrecovery_", "backup_")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSettingsUnreadable, err)
	}
	return &frozen{base: s.settings, vals: vals}, nil
}

func (f *frozen) Get(k string) (string, error) {
	if v, ok := f.vals[k]; ok {
		return v, nil
	}
	return "", recoveryclient.ErrNotFound
}

func (f *frozen) Set(k, v string) error {
	if err := f.base.Set(k, v); err != nil {
		return err
	}
	f.vals[k] = v
	return nil
}

func (f *frozen) Delete(k string) error {
	if err := f.base.Delete(k); err != nil {
		return err
	}
	delete(f.vals, k)
	return nil
}

// withRunLock runs a run or export as the single holder of backup-run on PostgreSQL; its
// context ends if the lock is lost. Contention is ErrInProgress.
func (s *Service) withRunLock(ctx context.Context, work func(context.Context) error) error {
	if !dbbackend.Postgres() {
		return work(ctx)
	}
	won, err := s.store.WithLock(ctx, "backup-run", work)
	if err != nil {
		return err
	}
	if !won {
		return recoveryclient.ErrInProgress
	}
	return nil
}

// withPinLock serialises pin, pairing and unpair on PostgreSQL, waiting up to pinLockWait.
func (s *Service) withPinLock(work func() error) error {
	if !dbbackend.Postgres() {
		return work()
	}
	ctx, cancel := context.WithTimeout(context.Background(), pinLockWait)
	defer cancel()
	return s.store.Serialized(ctx, "recovery-pin", func(context.Context) error { return work() })
}

type anchorRecord struct {
	PublicKey   string `json:"publicKey"` // base64 of the raw public key
	Threshold   int    `json:"threshold"`
	TotalShares int    `json:"totalShares"`
	// PinnedAt (table mode, Unix ns) orders pins: a host never moves its recovery.pub to an
	// older one, so a replayed retired record cannot roll it back.
	PinnedAt int64 `json:"pinnedAt,omitempty"`
}

// pinMarkFile, in STATE_DIR on each host, holds the newest PinnedAt this host accepted.
const pinMarkFile = "recovery-pin.at"

func (s *Service) pinMark() (int64, error) {
	if !s.table {
		return 0, nil
	}
	b, err := os.ReadFile(filepath.Join(s.dirs.State, pinMarkFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(b), 10, 64)
}

// raisePinMark records at as this host's newest accepted pin (table mode only).
func (s *Service) raisePinMark(at int64) error {
	mark, err := s.pinMark()
	if err != nil || at <= mark {
		return err
	}
	return fsutil.AtomicWriteFile(filepath.Join(s.dirs.State, pinMarkFile), []byte(strconv.FormatInt(at, 10)), 0o600)
}

func (s *Service) anchorAEAD() (cipher.AEAD, error) {
	master, err := cryptutil.LoadKey(s.secretPath("TOTP_SECRET_KEY_FILE", "totp-secret.key"))
	if err != nil {
		return nil, fmt.Errorf("the recovery key record needs the existing TOTP master key: %w", err)
	}
	key, err := hkdf.Key(sha256.New, master, nil, anchorLabel, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealAnchor seals k with its key ID as associated data: a database writer without the key
// file can neither read the record nor move it under another key ID.
func (s *Service) sealAnchor(k recoveryclient.RecoveryKey) (string, int64, error) {
	aead, err := s.anchorAEAD()
	if err != nil {
		return "", 0, err
	}
	rec := anchorRecord{PublicKey: base64.StdEncoding.EncodeToString(k.Public.Bytes()), Threshold: k.Threshold, TotalShares: k.TotalShares}
	if s.table {
		rec.PinnedAt = time.Now().UnixNano()
	}
	plain, _ := json.Marshal(rec)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(k.Public.ID()))), rec.PinnedAt, nil
}

func (s *Service) openAnchor(sealed, keyID string) (recoveryclient.RecoveryKey, int64, error) {
	aead, err := s.anchorAEAD()
	if err != nil {
		return recoveryclient.RecoveryKey{}, 0, err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < aead.NonceSize() {
		return recoveryclient.RecoveryKey{}, 0, fmt.Errorf("%w: the record is malformed", ErrAnchor)
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(keyID))
	if err != nil {
		return recoveryclient.RecoveryKey{}, 0, fmt.Errorf("%w: the record does not open under the pinned key ID", ErrAnchor)
	}
	var rec anchorRecord
	if err := json.Unmarshal(plain, &rec); err != nil {
		return recoveryclient.RecoveryKey{}, 0, fmt.Errorf("%w: %v", ErrAnchor, err)
	}
	pub, err := base64.StdEncoding.DecodeString(rec.PublicKey)
	if err != nil {
		return recoveryclient.RecoveryKey{}, 0, fmt.Errorf("%w: %v", ErrAnchor, err)
	}
	pk, err := recoverykey.ParsePublicKey(pub)
	if err != nil || pk.ID() != keyID {
		return recoveryclient.RecoveryKey{}, 0, fmt.Errorf("%w: the record holds another key", ErrAnchor)
	}
	return recoveryclient.RecoveryKey{Public: pk, Threshold: rec.Threshold, TotalShares: rec.TotalShares}, rec.PinnedAt, nil
}

// recordAnchor stores the sealed record of the pinned key (PostgreSQL only).
func (s *Service) recordAnchor(k recoveryclient.RecoveryKey) error {
	if !dbbackend.Postgres() {
		return nil
	}
	sealed, at, err := s.sealAnchor(k)
	if err == nil {
		err = s.settings.Set(anchorSetting, sealed)
	}
	if err == nil {
		err = s.raisePinMark(at)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPinNotRecorded, err)
	}
	return nil
}

// SyncRecoveryKey checks the pinned key against its sealed record on PostgreSQL and writes a
// missing recovery.pub from it. A record that does not open, a database pin without one, or a
// recovery.pub naming another key refuses (ErrAnchor) and writes nothing. Run at startup and
// before every run and export. Nothing pinned is fine; SQLite does nothing.
func (s *Service) SyncRecoveryKey() error {
	if !dbbackend.Postgres() {
		return nil
	}
	f, err := s.snapshot()
	if err != nil {
		return err
	}
	keyID, pinned := f.vals["kyrecovery_key_id"]
	sealed, recorded := f.vals[anchorSetting]
	switch {
	case !pinned && !recorded:
		return nil
	case !pinned:
		return fmt.Errorf("%w: a record exists without a pin", ErrAnchor)
	case !recorded:
		// A pin migrated or restored from SQLite, or one whose record write failed: record it
		// from recovery.pub if that file is the pinned key. A database writer cannot place
		// the file, so it cannot substitute a key this way.
		k, err := recoveryclient.LoadRecoveryKey(s.dirs.State, f)
		if err != nil {
			return fmt.Errorf("%w: the pin has no sealed record and recovery.pub is not the pinned key; pin the key again", ErrAnchor)
		}
		return s.recordAnchor(k)
	}
	k, at, err := s.openAnchor(sealed, keyID)
	if err != nil {
		return err
	}
	if t, e1 := strconv.Atoi(f.vals["kyrecovery_threshold"]); e1 != nil || t != k.Threshold {
		return fmt.Errorf("%w: the pinned topology differs from the record", ErrAnchor)
	}
	if n, e2 := strconv.Atoi(f.vals["kyrecovery_total_shares"]); e2 != nil || n != k.TotalShares {
		return fmt.Errorf("%w: the pinned topology differs from the record", ErrAnchor)
	}
	mark, err := s.pinMark()
	if err != nil {
		return fmt.Errorf("%w: %s is unreadable: %v", ErrAnchor, pinMarkFile, err)
	}
	path := recoveryclient.RecoveryKeyPath(s.dirs.State)
	if _, err := os.Stat(path); err == nil {
		if _, err := recoveryclient.LoadRecoveryKey(s.dirs.State, f); err == nil {
			return s.raisePinMark(at)
		} else if !s.table {
			return fmt.Errorf("%w: %s names another key", ErrAnchor, path)
		}
		// Table mode (MH5): every host keeps its own recovery.pub, so a re-pin on another
		// host leaves this one stale. Only a strictly newer pin than this host accepted may
		// replace it: a database writer can replay a retired record, never forge PinnedAt.
		if at <= mark {
			return fmt.Errorf("%w: %s names another key and the record is not a newer pin", ErrAnchor, path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if s.table && at < mark {
		return fmt.Errorf("%w: the record is an older pin than this host accepted", ErrAnchor)
	}
	// Missing on this replica: write it from the record, write-once, as a pin would.
	if err := recoveryclient.StoreRecoveryKey(s.dirs.State, f, k); err != nil {
		return err
	}
	return s.raisePinMark(at)
}

// checkAnchor reports, for status, a pin whose sealed record is missing or does not open.
func (s *Service) checkAnchor(f *frozen) error {
	keyID, pinned := f.vals["kyrecovery_key_id"]
	sealed, recorded := f.vals[anchorSetting]
	switch {
	case !pinned:
		return nil
	case !recorded:
		return ErrPinNotRecorded
	}
	_, _, err := s.openAnchor(sealed, keyID)
	return err
}

// pinTopology refuses a re-pin of the pinned key with another topology; the same key and
// topology is a no-op success.
func pinTopology(f recoveryclient.Settings, k recoveryclient.RecoveryKey) error {
	id, err := f.Get("kyrecovery_key_id")
	if errors.Is(err, recoveryclient.ErrNotFound) || err == nil && id != k.Public.ID() {
		return nil // nothing pinned, or another key: StoreRecoveryKey decides
	}
	if err != nil {
		return err
	}
	t, _ := f.Get("kyrecovery_threshold")
	n, _ := f.Get("kyrecovery_total_shares")
	if t != strconv.Itoa(k.Threshold) || n != strconv.Itoa(k.TotalShares) {
		return ErrTopologyPinned
	}
	return nil
}

// RefusePinWithoutKey refuses a stopped SQLite install (a migration source or restore
// staging) whose global state.db pins a recovery key that recovery.pub does not hold: the
// pin could never be honoured, and nothing has been copied yet.
func RefusePinWithoutKey(stateDir string) error {
	path := filepath.Join(stateDir, "state.db")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := dbbackend.OpenSQLiteFile(path, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer db.Close()
	var id string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'kyrecovery_key_id'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(recoveryclient.RecoveryKeyPath(stateDir))
	if err != nil {
		return fmt.Errorf("a recovery key is pinned (%s) but %s is missing; restore it before copying", id, recoveryclient.RecoveryKeyPath(stateDir))
	}
	if pk, err := recoverykey.ParsePublicKey(raw); err != nil || pk.ID() != id {
		return fmt.Errorf("a recovery key is pinned (%s) but %s holds another; restore the right file before copying", id, recoveryclient.RecoveryKeyPath(stateDir))
	}
	return nil
}
