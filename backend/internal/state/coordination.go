package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"golang.org/x/sys/unix"
)

// Coordination is what PostgreSQL replicas share beyond the mail itself, kept in the global
// state.db: rate windows, spent CAPTCHA challenges and replay IDs on database time, and
// single-owner locks. SQLite keeps the windows, challenges and replay IDs in process memory
// and never calls those; its locks are flocks beside state.db.

// nowMillis is database time in milliseconds: no replica's clock defines a shared window.
func nowMillis() string {
	return dbbackend.SQL("CAST(unixepoch('subsec') * 1000 AS INTEGER)", "CAST(EXTRACT(EPOCH FROM statement_timestamp()) * 1000 AS BIGINT)")
}

var errInvalidKey = errors.New("state: invalid coordination key")

// TakeAttempt counts one hit in key's window and reports whether it is within limit, with the
// window's generation (its expiry, for ReleaseAttempt) and the time left in it.
func (s *Store) TakeAttempt(ctx context.Context, key string, limit int, window time.Duration) (gen int64, left time.Duration, ok bool, err error) {
	if key == "" || len(key) > 400 || limit < 1 || window < time.Millisecond || window > 7*24*time.Hour {
		return 0, 0, false, errInvalidKey
	}
	now := nowMillis()
	// hits stops at limit+1: a full window stays full without growing.
	q := `INSERT INTO security_windows (key, hits, expires_at) VALUES (?, 1, ` + now + ` + ?)
ON CONFLICT (key) DO UPDATE SET
hits = CASE WHEN security_windows.expires_at <= ` + now + ` THEN 1
            WHEN security_windows.hits <= ? THEN security_windows.hits + 1 ELSE security_windows.hits END,
expires_at = CASE WHEN security_windows.expires_at <= ` + now + ` THEN ` + now + ` + ? ELSE security_windows.expires_at END
RETURNING hits, expires_at, expires_at - ` + now
	var hits, ms int64
	w := window.Milliseconds()
	if err := s.db.QueryRowContext(ctx, q, key, w, limit, w).Scan(&hits, &gen, &ms); err != nil {
		return 0, 0, false, err
	}
	return gen, time.Duration(ms) * time.Millisecond, hits <= int64(limit), nil
}

// ReleaseAttempt hands one hit back to the window gen, never to a newer one.
func (s *Store) ReleaseAttempt(ctx context.Context, key string, gen int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE security_windows SET hits = hits - 1 WHERE key = ? AND expires_at = ? AND hits > 0`, key, gen)
	return err
}

// ClearAttempts forgets key's window: a success clears its strikes.
func (s *Store) ClearAttempts(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM security_windows WHERE key = ?`, key)
	return err
}

// AttemptHits reads key's live window without counting.
func (s *Store) AttemptHits(ctx context.Context, key string) (hits int, left time.Duration, err error) {
	var h, l int64
	err = s.db.QueryRowContext(ctx, `SELECT hits, expires_at - `+nowMillis()+` FROM security_windows WHERE key = ? AND expires_at > `+nowMillis(), key).Scan(&h, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return int(h), time.Duration(l) * time.Millisecond, err
}

// SpendChallenge records a challenge until expiresMs (unix milliseconds) and reports whether
// it was unspent. An expired record is replaced.
func (s *Store) SpendChallenge(ctx context.Context, salt string, expiresMs int64) (bool, error) {
	if salt == "" || len(salt) > 128 {
		return false, errInvalidKey
	}
	var got string
	err := s.db.QueryRowContext(ctx, `INSERT INTO spent_challenges (salt, expires_at) VALUES (?, ?)
ON CONFLICT (salt) DO UPDATE SET expires_at = excluded.expires_at WHERE spent_challenges.expires_at <= `+nowMillis()+`
RETURNING salt`, salt, expiresMs).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RecordReplay records id for window as attempt and reports whether it was new; an expired
// record is taken over.
func (s *Store) RecordReplay(ctx context.Context, id, attempt string, window time.Duration) (bool, error) {
	if id == "" || len(id) > 400 || attempt == "" {
		return false, errInvalidKey
	}
	now := nowMillis()
	var got string
	err := s.db.QueryRowContext(ctx, `INSERT INTO replay_ids (id, attempt, expires_at) VALUES (?, ?, `+now+` + ?)
ON CONFLICT (id) DO UPDATE SET expires_at = excluded.expires_at, attempt = excluded.attempt WHERE replay_ids.expires_at <= `+now+`
RETURNING id`, id, attempt, window.Milliseconds()).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// UndoReplay releases id only if attempt wrote its record: an attempt whose outcome is
// unknown never releases an earlier request's record.
func (s *Store) UndoReplay(ctx context.Context, id, attempt string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM replay_ids WHERE id = ? AND attempt = ?`, id, attempt)
	return err
}

// CleanExpiredCoordination deletes expired windows, challenges, replay IDs, sessions and
// one-time authority.
func (s *Store) CleanExpiredCoordination(ctx context.Context) error {
	for _, table := range []string{"security_windows", "spent_challenges", "replay_ids", "sessions", "auth_challenges"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE expires_at < "+nowMillis()); err != nil {
			return err
		}
	}
	return nil
}

// Lock bounds (variables so tests can shorten them). Acquisition is a connection checkout plus
// pg_try_advisory_lock; a probe that does not answer within lockProbeTimeout ends ownership at
// once, without waiting for the driver to give up on the connection.
var (
	lockAcquireTimeout = 2 * time.Second
	lockProbeInterval  = time.Second
	lockProbeTimeout   = time.Second
)

// ProbeBound is the longest a lost lock goes unnoticed: one probe interval plus its timeout. A
// new owner waits longer than this before its first job.
func ProbeBound() time.Duration { return lockProbeInterval + lockProbeTimeout }

var (
	// ErrLockAcquire is a lock attempt that could not reach the database in time.
	ErrLockAcquire = errors.New("state: lock acquisition timed out")
	// ErrOwnershipLost is the cause of an owned context the probe ended.
	ErrOwnershipLost = errors.New("state: lock ownership lost")
)

// Advisory key spaces (two-int form, so they never meet the writer's bigint locks), each
// scoped to the schema so tests sharing one PostgreSQL never contend.
const (
	spaceLock     = 3 // WithLock and Serialized
	spaceInstance = 4 // a live process (RegisterInstance)
)

func advisoryKey(space int) string {
	return fmt.Sprintf(`%d, hashtext(current_schema() || ':' || $1)`, space)
}

// WithLock runs work as the single holder of name and reports whether it won. PostgreSQL: a
// session advisory lock on a connection of its own, discarded afterwards so the lock never
// returns to the pool (use a direct connection or session pooling, never transaction
// pooling). Taking it is bounded by lockAcquireTimeout. While work runs the connection is
// probed every lockProbeInterval; a probe that fails or does not answer within
// lockProbeTimeout cancels work's context with ErrOwnershipLost. Release order: cancel, wait
// for work, stop the probe, discard the connection. SQLite: a flock beside state.db.
func (s *Store) WithLock(ctx context.Context, name string, work func(context.Context) error) (bool, error) {
	if !dbbackend.Postgres() {
		return withFlock(ctx, s.lockBase(), name, work)
	}
	acquire, cancelAcquire := context.WithTimeout(ctx, lockAcquireTimeout)
	defer cancelAcquire()
	conn, won, err := s.tryLock(acquire, spaceLock, name)
	if err != nil || !won {
		if err != nil && acquire.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w: %v", ErrLockAcquire, err)
		}
		return false, err
	}
	defer discard(conn)
	owned, cancel := context.WithCancelCause(ctx)
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		probe(owned, conn, cancel)
	}()
	err = work(owned)
	cancel(nil)
	<-probeDone
	return true, err
}

func (s *Store) lockBase() string { return filepath.Join(s.baseDir, "state.db") }

// tryLock checks out a connection and tries the lock, both under ctx. The driver may finish
// the query after ctx ends, so the result is awaited in the background and ctx alone decides.
func (s *Store) tryLock(ctx context.Context, space int, name string) (*sql.Conn, bool, error) {
	type result struct {
		conn *sql.Conn
		won  bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			done <- result{err: err}
			return
		}
		var won bool
		err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(`+advisoryKey(space)+`)`, name).Scan(&won)
		done <- result{conn, won, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.won {
			if r.conn != nil {
				discard(r.conn)
			}
			return nil, false, r.err
		}
		return r.conn, true, nil
	case <-ctx.Done():
		go func() { // whatever arrives later is released with its connection
			if r := <-done; r.conn != nil {
				discard(r.conn)
			}
		}()
		return nil, false, ctx.Err()
	}
}

func probe(owned context.Context, conn *sql.Conn, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(lockProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-owned.Done():
			return
		case <-ticker.C:
		}
		ping, stop := context.WithTimeout(owned, lockProbeTimeout)
		res := make(chan error, 1)
		go func() { res <- conn.PingContext(ping) }()
		var err error
		select {
		case err = <-res:
		case <-ping.Done():
			err = ping.Err()
		}
		stop()
		if err != nil && owned.Err() == nil {
			cancel(fmt.Errorf("%w: %v", ErrOwnershipLost, err))
			return
		}
	}
}

// discard closes conn's physical connection instead of pooling it: closing the session
// releases every lock it holds, even if an unlock would fail.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// Serialized runs work while holding name, waiting for another holder. PostgreSQL only;
// SQLite runs work directly (its writers already serialize on the file).
func (s *Store) Serialized(ctx context.Context, name string, work func(context.Context) error) error {
	if !dbbackend.Postgres() {
		return work(ctx)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer discard(conn)
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(`+advisoryKey(spaceLock)+`)`, name); err != nil {
		return err
	}
	return work(ctx)
}

// withFlock holds an exclusive flock on a persistent file beside the database for name.
// Closing the descriptor (or process exit) releases it; the inode is never unlinked.
func withFlock(ctx context.Context, dbPath, name string, work func(context.Context) error) (bool, error) {
	path := fmt.Sprintf("%s.%x.lock", dbPath, sha256.Sum256([]byte(name)))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	return true, work(ctx)
}

// InstanceLockFile is the SQLite server-instance lock beside the global state.db: the api and
// daemon hold it shared while they run, and migrate-to-postgres takes it exclusively.
const InstanceLockFile = "server-instance.lock"

// ErrInstanceRunning refuses an exclusive instance lock while a server holds it.
var ErrInstanceRunning = errors.New("KyPost is running on this data: stop it first (docker compose stop app)")

// HoldInstance takes the server-instance flock in stateDir, shared (a server process) or
// exclusive (migrate-to-postgres), and returns its release. Exclusive never waits.
func HoldInstance(stateDir string, exclusive bool) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(stateDir, InstanceLockFile), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX | unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrInstanceRunning
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
