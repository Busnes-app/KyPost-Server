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

// Fence names; a per-ID kind's name is its prefix plus the org, user or mailbox ID. The
// kinds are taken in fenceOrder.
const (
	FenceOrg             = "fence:org:"
	FenceSettings        = "fence:settings:"
	FenceNativeDomains   = "fence:native-domains"
	FenceSSOLifecycle    = "fence:sso-lifecycle"
	FenceMailbox         = "fence:mailbox:"
	FenceQuota           = "fence:quota:"
	FenceSSO             = "fence:sso"
	FenceBackupOperation = "fence:backup-operation"
)

// fenceOrder is the fixed acquisition order by kind. settings -> native-domains ->
// sso-lifecycle -> quota is today's flock order (api/masked_addresses.go,
// sso/native_mailbox.go, app/receiving.go); where org, mailbox, sso and backup-operation
// sit is MH4's to confirm.
var fenceOrder = []string{"org", "settings", "native-domains", "sso-lifecycle", "mailbox", "quota", "sso", "backup-operation"}

// fencePool is each Store's own pool of fence sessions, opened on first use. A held fence
// keeps its connection for the whole section, so sharing the store's small pool would let
// fenced requests starve every other query, their own included. maxFenceSessions caps what
// one process holds (provisional until the capacity measurement); a set that cannot get a
// session within fenceWait is ErrFenceWait.
type fencePool struct {
	once sync.Once
	mu   sync.Mutex
	db   *sql.DB
	err  error
}

const maxFenceSessions = 32

func (p *fencePool) get(baseDir string) (*sql.DB, error) {
	p.once.Do(func() {
		schema, _, err := dbbackend.SchemaName(filepath.Join(baseDir, "state.db"))
		var db *sql.DB
		if err == nil {
			db, err = dbbackend.Pool(schema, false)
		}
		if err == nil {
			db.SetMaxOpenConns(maxFenceSessions)
			db.SetMaxIdleConns(0) // every session is discarded on release
		}
		p.mu.Lock()
		p.db, p.err = db, err
		p.mu.Unlock()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db, p.err
}

func (p *fencePool) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return nil
	}
	return p.db.Close()
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

// WithFences runs work holding every fence of the set on one dedicated session, taken in
// fenceOrder (then by name), so fences within one request never wait on each other,
// including two whose keys collide. The whole set is acquired within fenceWait or the call
// fails with ErrFenceWait. While work runs the session is probed as WithLock's is: a probe
// that fails cancels work's context with ErrOwnershipLost. The session is discarded
// afterwards, which releases every fence.
func (s *Store) WithFences(ctx context.Context, fences []Fence, work func(context.Context) error) error {
	if !dbbackend.Postgres() {
		return errFencePG
	}
	rank := make(map[string]int, len(fences))
	for _, f := range fences {
		kind := fenceKind(f.Name)
		if _, dup := rank[f.Name]; kind == "" || dup {
			return errFence
		}
		rank[f.Name] = slices.Index(fenceOrder, kind)
	}
	if len(fences) == 0 {
		return errFence
	}
	set := slices.SortedFunc(slices.Values(fences), func(a, b Fence) int {
		return cmp.Or(cmp.Compare(rank[a.Name], rank[b.Name]), strings.Compare(a.Name, b.Name))
	})
	wait, cancelWait := context.WithTimeout(ctx, fenceWait)
	defer cancelWait()
	pool, err := s.fences.get(s.baseDir)
	if err != nil {
		return err
	}
	conn, err := pool.Conn(wait)
	if wait.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("%w: no fence session", ErrFenceWait)
	}
	if err == nil {
		// lock_timeout backs the context up on the server; the session is discarded after. A lock
		// timeout (55P03) or a deadlock PostgreSQL detects (40P01) is a bounded wait too.
		_, err = conn.ExecContext(wait, `SELECT set_config('lock_timeout', $1, false)`, fmt.Sprintf("%dms", fenceWait.Milliseconds()))
	}
	for _, f := range set {
		if err != nil {
			break
		}
		fn := "pg_advisory_lock"
		if f.Shared {
			fn = "pg_advisory_lock_shared"
		}
		if _, err = conn.ExecContext(wait, `SELECT `+fn+`(`+fenceKey+`)`, fenceName(f.Name)); err != nil {
			var pg *pgconn.PgError
			if wait.Err() != nil && ctx.Err() == nil || errors.As(err, &pg) && (pg.Code == "55P03" || pg.Code == "40P01") {
				err = fmt.Errorf("%w: %s", ErrFenceWait, fenceKind(f.Name))
			}
		}
	}
	if err != nil {
		if conn != nil {
			discard(conn)
		}
		return err
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
	return err
}
