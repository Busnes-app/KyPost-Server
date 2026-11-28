package state

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/jackc/pgx/v5/pgconn"
)

// Fences replace the host-local flocks in multi mode (docs/superpowers/specs/
// 2026-11-22-multi-host-replicas-design.md, "Fences"). A fence orders work; the documents'
// revisions keep it safe when a fence is lost. PostgreSQL only.

// Fence is one fence of a set: its name and whether it is taken shared.
type Fence struct {
	Name   string
	Shared bool
}

// fenceOrder is the fixed acquisition order by kind, confirmed against every call site
// (MH4): the org fence is outermost; native outbound, relay and recovery take the SSO
// settings fence after the domains and before the directory.
var fenceOrder = []string{"org", "settings", "native-domains", "sso", "sso-lifecycle", "mailbox", "quota", "backup-operation"}

// fencePools holds one handle for fence sessions per schema, shared by every Store in the
// process. Only a successful open is kept, and a handle is never closed: it keeps no idle
// connection. A held fence keeps its connection, borrowed from the process's shared pool, for
// the whole section, and a fenced request borrows more under it (a read transaction, then a
// statement), so fence sessions across every schema may take at most a quarter of that pool
// (fenceSlots): fenced requests cannot take every connection and wait on each other for one
// more. A set that cannot get a session within fenceWait is ErrFenceWait.
var fencePools struct {
	sync.Mutex
	bySchema map[string]*sql.DB
	slots    chan struct{}
}

// fenceSlots is the process's semaphore of fence sessions: a quarter of the pool, at least 1.
func fenceSlots() (chan struct{}, error) {
	fencePools.Lock()
	defer fencePools.Unlock()
	if fencePools.slots == nil {
		size, err := dbbackend.PoolSize()
		if err != nil {
			return nil, err
		}
		fencePools.slots = make(chan struct{}, max(1, size/4))
	}
	return fencePools.slots, nil
}

func fencePool(baseDir string) (*sql.DB, error) {
	schema, _, err := dbbackend.SchemaName(filepath.Join(baseDir, "state.db"))
	if err != nil {
		return nil, err
	}
	// Keyed by server and database too: a test may point the process at a new database.
	key := dbbackend.Describe() + "|" + schema
	fencePools.Lock()
	defer fencePools.Unlock()
	if db := fencePools.bySchema[key]; db != nil {
		return db, nil
	}
	db, err := dbbackend.Pool(schema, false)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(0) // every session is discarded on release
	if fencePools.bySchema == nil {
		fencePools.bySchema = map[string]*sql.DB{}
	}
	fencePools.bySchema[key] = db
	return db, nil
}

// fenceWait bounds the wait for a whole fence set (R's pairingLockWait). A variable so tests
// can shorten it.
var fenceWait = 10 * time.Second

var (
	// ErrFenceWait is a fence set not acquired within fenceWait; callers answer 503. It
	// names the kind only: a name carries a user or mailbox ID.
	ErrFenceWait = errors.New("state: fence wait timed out")
	errFence     = errors.New("state: invalid fence set")
	errFencePG   = errors.New("state: fences need PostgreSQL")
)

// fenceKind is a fence name's kind, "" for a name that is not a fence.
func fenceKind(name string) string {
	rest, ok := strings.CutPrefix(name, "fence:")
	if !ok || len(name) > 400 {
		return ""
	}
	kind, id, scoped := strings.Cut(rest, ":")
	perID := kind == "org" || kind == "settings" || kind == "mailbox" || kind == "quota"
	if !slices.Contains(fenceOrder, kind) || scoped != perID || scoped && id == "" {
		return ""
	}
	return kind
}

// fenceKey is the 64-bit advisory key: the first 8 bytes of SHA-256(schema ":" name). The
// bigint form is a key space apart from R's two-int locks. fenceName is a test hook that
// forces collisions.
const fenceKey = `('x' || left(encode(sha256(convert_to(current_schema() || ':' || $1, 'UTF8')), 'hex'), 16))::bit(64)::bigint`

var fenceName = func(name string) string { return name }

// WithFences runs work holding every fence of the set (HoldFences), then releases it.
func (s *Store) WithFences(ctx context.Context, fences []Fence, work func(context.Context) error) error {
	owned, release, err := s.HoldFences(ctx, fences)
	if err != nil {
		return err
	}
	defer release()
	return work(owned)
}

// fenceSession is the one session every fence of a request is held on. mu is held while a
// nested set waits on conn, so the probe skips that tick rather than reading a blocked
// connection as a lost one; the waiting holder writes nothing meanwhile.
type fenceSession struct {
	schema string
	conn   *sql.Conn
	mu     sync.Mutex
	lost   context.CancelCauseFunc // ends ownership: a fence a failed unlock left held
}

// heldFences is what an owned context carries: its session and the highest kind rank held.
type heldFences struct {
	session *fenceSession
	rank    int
}

type heldFencesKey struct{}

// HoldFences takes every fence of the set on one dedicated session (fencePool), in fenceOrder
// (then by name), so fences within one request never wait on each other, including two
// whose keys collide. A set taken under a context HoldFences returned joins that session
// (re-entrant there); it must not rank before a fence already held. Each set is acquired
// within fenceWait or the call fails with ErrFenceWait. Until release the session is probed
// as WithLock's is: a probe that fails cancels the owned context with ErrOwnershipLost. The
// outermost release discards the session, which releases every fence; a nested release
// unlocks only its own set. release is idempotent.
func (s *Store) HoldFences(ctx context.Context, fences []Fence) (context.Context, func(), error) {
	if !dbbackend.Postgres() {
		return nil, nil, errFencePG
	}
	rank := make(map[string]int, len(fences))
	for _, f := range fences {
		kind := fenceKind(f.Name)
		if _, dup := rank[f.Name]; kind == "" || dup {
			return nil, nil, errFence
		}
		rank[f.Name] = slices.Index(fenceOrder, kind)
	}
	if len(fences) == 0 {
		return nil, nil, errFence
	}
	set := slices.SortedFunc(slices.Values(fences), func(a, b Fence) int {
		return cmp.Or(cmp.Compare(rank[a.Name], rank[b.Name]), strings.Compare(a.Name, b.Name))
	})
	top := rank[set[len(set)-1].Name]
	schema, _, err := dbbackend.SchemaName(filepath.Join(s.baseDir, "state.db"))
	if err != nil {
		return nil, nil, err
	}
	if held, ok := ctx.Value(heldFencesKey{}).(heldFences); ok && held.session.schema == schema {
		if rank[set[0].Name] < held.rank {
			return nil, nil, errFence
		}
		return held.session.nest(ctx, set, max(top, held.rank))
	}
	wait, cancelWait := context.WithTimeout(ctx, fenceWait)
	defer cancelWait()
	pool, err := fencePool(s.baseDir)
	if err != nil {
		return nil, nil, err
	}
	slots, err := fenceSlots()
	if err != nil {
		return nil, nil, err
	}
	var conn *sql.Conn
	select {
	case slots <- struct{}{}:
		conn, err = pool.Conn(wait)
		if err != nil {
			<-slots
		}
	case <-wait.Done():
		err = wait.Err()
	}
	if wait.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("%w: no fence session", ErrFenceWait)
	}
	if err == nil {
		// lock_timeout backs the context up on the server; the session is discarded after. A lock
		// timeout (55P03) or a deadlock PostgreSQL detects (40P01) is a bounded wait too.
		_, err = conn.ExecContext(wait, `SELECT set_config('lock_timeout', $1, false)`, fmt.Sprintf("%dms", fenceWait.Milliseconds()))
	}
	if err == nil {
		err = lockFences(ctx, wait, conn, set)
	}
	if err != nil {
		if conn != nil {
			discard(conn)
			<-slots
		}
		return nil, nil, err
	}
	owned, cancel := context.WithCancelCause(ctx)
	session := &fenceSession{schema: schema, conn: conn, lost: cancel}
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		probe(owned, conn, cancel, &session.mu)
	}()
	var once sync.Once
	return context.WithValue(owned, heldFencesKey{}, heldFences{session, top}), func() {
		once.Do(func() {
			cancel(nil)
			<-probeDone
			discard(conn)
			<-slots
		})
	}, nil
}

// nest takes set on an already held session. Its wait outlasts lock_timeout by a second, so
// PostgreSQL ends a long wait with 55P03 and the session survives it.
func (f *fenceSession) nest(ctx context.Context, set []Fence, rank int) (context.Context, func(), error) {
	wait, cancelWait := context.WithTimeout(ctx, fenceWait+time.Second)
	defer cancelWait()
	f.mu.Lock()
	err := lockFences(ctx, wait, f.conn, set)
	f.mu.Unlock()
	if errors.Is(err, errFenceStuck) {
		f.lost(fmt.Errorf("%w: %v", ErrOwnershipLost, err))
	}
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return context.WithValue(ctx, heldFencesKey{}, heldFences{f, rank}), func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			// A fence a failed unlock left held is released only by the outermost discard,
			// so the owners stop now rather than working past a fence they still hold.
			if err := unlockFences(f.conn, set); err != nil {
				f.lost(fmt.Errorf("%w: fence release: %v", ErrOwnershipLost, err))
			}
		})
	}, nil
}

// lockFences takes set in order on conn. A set that fails part way is released by the
// caller: the outermost by discarding the session, a nested one here.
func lockFences(ctx, wait context.Context, conn *sql.Conn, set []Fence) error {
	for i, f := range set {
		fn := "pg_advisory_lock"
		if f.Shared {
			fn = "pg_advisory_lock_shared"
		}
		if _, err := conn.ExecContext(wait, `SELECT `+fn+`(`+fenceKey+`)`, fenceName(f.Name)); err != nil {
			var pg *pgconn.PgError
			if wait.Err() != nil && ctx.Err() == nil || errors.As(err, &pg) && (pg.Code == "55P03" || pg.Code == "40P01") {
				err = fmt.Errorf("%w: %s", ErrFenceWait, fenceKind(f.Name))
			}
			if uerr := unlockFences(conn, set[:i]); uerr != nil {
				return errors.Join(err, errFenceStuck, uerr)
			}
			return err
		}
	}
	return nil
}

// errFenceStuck is a partly acquired set whose cleanup failed: the session still holds part.
var errFenceStuck = errors.New("state: fence cleanup failed")

// fenceUnlockSQL is one unlock statement; a variable so a test can stall it on the server.
var fenceUnlockSQL = func(shared bool) string {
	if shared {
		return `SELECT pg_advisory_unlock_shared(` + fenceKey + `)`
	}
	return `SELECT pg_advisory_unlock(` + fenceKey + `)`
}

// unlockFences releases set on conn, newest first, within lockProbeTimeout in all: a stalled
// connection must not hold the session mutex (and so the probe) indefinitely.
func unlockFences(conn *sql.Conn, set []Fence) error {
	ctx, cancel := context.WithTimeout(context.Background(), lockProbeTimeout)
	defer cancel()
	for _, f := range slices.Backward(set) {
		if _, err := conn.ExecContext(ctx, fenceUnlockSQL(f.Shared), fenceName(f.Name)); err != nil {
			return err
		}
	}
	return nil
}

// VolFence is vol's FenceFunc for the table mounts of the instance whose state is stateDir.
// It opens nothing until a fence is taken.
func VolFence(stateDir string) vol.FenceFunc {
	s := &Store{baseDir: stateDir}
	return func(ctx context.Context, name string, shared bool) (context.Context, func(), error) {
		return s.HoldFences(ctx, []Fence{{Name: name, Shared: shared}})
	}
}
