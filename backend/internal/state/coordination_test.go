package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgtest"
)

// twoReplicas opens two handles on one global state.db: two processes sharing the database.
func twoReplicas(t *testing.T) (*Store, *Store) {
	t.Helper()
	dir := t.TempDir()
	a, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestSharedWindowAdmitsTheLimitAcrossReplicas(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			_, _, ok, err := s.TakeAttempt(ctx, "dav:198.51.100.7", 10, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := admitted.Load(); got != 10 {
		t.Fatalf("admitted %d of 40 across two replicas, want exactly 10", got)
	}
	hits, left, err := b.AttemptHits(ctx, "dav:198.51.100.7")
	if err != nil || hits != 11 || left <= 0 {
		t.Fatalf("AttemptHits = %d, %v, %v; want a full window (11) with time left", hits, left, err)
	}
	if err := a.ClearAttempts(ctx, "dav:198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	if hits, _, _ := b.AttemptHits(ctx, "dav:198.51.100.7"); hits != 0 {
		t.Fatalf("after ClearAttempts hits = %d", hits)
	}
}

// AttemptHitsUnder sums only the live windows under its prefix, on either replica.
func TestAttemptHitsUnderSumsOnePrefix(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	for _, k := range []string{"esc:ip1:acct1", "esc:ip1:acct1", "esc:ip1:acct2", "esc:ip10:acct1", "esc:ip2:acct1", "esc:ip1;"} {
		if _, _, _, err := a.TakeAttempt(ctx, k, 100, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := a.TakeAttempt(ctx, "esc:ip1:old", 100, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if n, err := b.AttemptHitsUnder(ctx, "esc:ip1:"); err != nil || n != 3 {
		t.Fatalf("hits under esc:ip1: = %d, %v; want 3", n, err)
	}
	if _, err := b.AttemptHitsUnder(ctx, ""); !errors.Is(err, errInvalidKey) {
		t.Fatalf("empty prefix: %v", err)
	}
}

// A refund for an expired window must not credit the window that replaced it.
func TestRefundRestoresOnlyItsOwnWindow(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	old, _, ok, err := a.TakeAttempt(ctx, "k", 2, time.Millisecond)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var gen int64
	for gen = old; gen == old; {
		if gen, _, _, err = b.TakeAttempt(ctx, "k", 2, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, _ := b.TakeAttempt(ctx, "k", 2, time.Minute); !ok {
		t.Fatal("second hit of the new window refused")
	}
	if err := a.ReleaseAttempt(ctx, "k", 2, old); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := b.TakeAttempt(ctx, "k", 2, time.Minute); ok {
		t.Fatal("a refund of the expired window credited the new one")
	}
	if err := a.ReleaseAttempt(ctx, "k", 2, gen); err != nil {
		t.Fatal(err)
	}
	// The window had refused attempts (hits at limit+1); its own refund admits exactly one more.
	if _, _, ok, _ := b.TakeAttempt(ctx, "k", 2, time.Minute); !ok {
		t.Fatal("own refund freed nothing")
	}
	if _, _, ok, _ := b.TakeAttempt(ctx, "k", 2, time.Minute); ok {
		t.Fatal("own refund freed two attempts")
	}
}

// A window that already refused attempts frees exactly one per refund.
func TestRefundAfterRefusalsFreesOne(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	var gen int64
	for i := range 5 {
		g, _, ok, err := a.TakeAttempt(ctx, "full", 3, time.Minute)
		if err != nil || ok != (i < 3) {
			t.Fatalf("take %d: ok=%v err=%v", i, ok, err)
		}
		if i == 0 {
			gen = g
		}
	}
	if err := b.ReleaseAttempt(ctx, "full", 3, gen); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := b.TakeAttempt(ctx, "full", 3, time.Minute); !ok {
		t.Fatal("a refund after refusals freed nothing")
	}
	if _, _, ok, _ := a.TakeAttempt(ctx, "full", 3, time.Minute); ok {
		t.Fatal("one refund freed two attempts")
	}
}

func TestReplayAndSpendAcrossReplicas(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	if ok, err := a.SpendChallenge(ctx, "salt", time.Now().Add(time.Hour).UnixMilli()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := b.SpendChallenge(ctx, "salt", time.Now().Add(time.Hour).UnixMilli()); ok || err != nil {
		t.Fatalf("spent twice: %v %v", ok, err)
	}
	if ok, _ := b.SpendChallenge(ctx, "old", 1); !ok {
		t.Fatal("first spend of an already expired salt refused")
	}
	if ok, _ := a.SpendChallenge(ctx, "old", time.Now().Add(time.Hour).UnixMilli()); !ok {
		t.Fatal("an expired spend still refuses")
	}

	if ok, err := a.RecordReplay(ctx, "event-1", "attempt-a", time.Hour); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := b.RecordReplay(ctx, "event-1", "attempt-b", time.Hour); ok {
		t.Fatal("replay accepted on the other replica")
	}
	// B's attempt failed ambiguously: its undo must not release A's record.
	if err := b.UndoReplay(ctx, "event-1", "attempt-b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.RecordReplay(ctx, "event-1", "attempt-c", time.Hour); ok {
		t.Fatal("another attempt's undo released the record")
	}
	if err := a.UndoReplay(ctx, "event-1", "attempt-a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.RecordReplay(ctx, "event-1", "attempt-d", time.Hour); !ok {
		t.Fatal("the writer's own undo did not release its record")
	}
}

func TestInvalidCoordinationKeysRefuse(t *testing.T) {
	a, _ := twoReplicas(t)
	ctx := context.Background()
	if _, _, _, err := a.TakeAttempt(ctx, "", 1, time.Second); !errors.Is(err, errInvalidKey) {
		t.Fatal(err)
	}
	if _, err := a.RecordReplay(ctx, "x", "", time.Second); !errors.Is(err, errInvalidKey) {
		t.Fatal(err)
	}
}

func TestWithLockHasOneHolder(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	inside := make(chan struct{})
	leave := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		won, err := a.WithLock(ctx, "role:daemon", func(context.Context) error {
			close(inside)
			<-leave
			return nil
		})
		if !won {
			err = errors.Join(err, errors.New("a lost an uncontended lock"))
		}
		done <- err
	}()
	<-inside
	if won, err := b.WithLock(ctx, "role:daemon", func(context.Context) error { return nil }); won || err != nil {
		t.Fatalf("second holder: won=%v err=%v", won, err)
	}
	if won, err := b.WithLock(ctx, "role:other", func(context.Context) error { return nil }); !won || err != nil {
		t.Fatalf("another name: won=%v err=%v", won, err)
	}
	close(leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if won, err := b.WithLock(ctx, "role:daemon", func(context.Context) error { return nil }); !won || err != nil {
		t.Fatalf("after release: won=%v err=%v", won, err)
	}
}

// Terminating the lock's session must cancel the holder within the probe bound.
func TestLockLossCancelsTheOwner(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	ctx := context.Background()
	inside := make(chan struct{})
	cancelled := make(chan time.Time, 1)
	go func() {
		_, _ = a.WithLock(ctx, "role:x", func(owned context.Context) error {
			close(inside)
			<-owned.Done()
			if !errors.Is(context.Cause(owned), ErrOwnershipLost) {
				t.Errorf("cause = %v", context.Cause(owned))
			}
			cancelled <- time.Now()
			return nil
		})
	}()
	<-inside
	killed := time.Now()
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND granted AND objsubid=2
 AND classid=3 AND objid=hashtext(current_schema() || ':' || 'role:x')::oid`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminate: %d %v", n, err)
	}
	select {
	case at := <-cancelled:
		if d := at.Sub(killed); d > 3*time.Second {
			t.Fatalf("owner cancelled after %v, want < 3s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner never cancelled after its session ended")
	}
	if won, err := b.WithLock(ctx, "role:x", func(context.Context) error { return nil }); !won || err != nil {
		t.Fatalf("takeover: won=%v err=%v", won, err)
	}
}

// A released lock is free when WithLock or Hold returns, not once PostgreSQL has reaped the
// closed session: a backup run straight after another one is not "already in progress"
// (CI flake, post 8148). Checked as each lock connection is discarded, so it is deterministic.
func TestReleasedLockIsFreeAtOnce(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	ctx := context.Background()
	var discards int
	a.unlocked = func() {
		var n int
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objsubid=2
 AND classid=3 AND objid=hashtext(current_schema() || ':' || 'backup-run')::oid`).Scan(&n); err != nil || n != 0 {
			t.Errorf("lock connection discarded with the lock still held: %d %v", n, err)
		}
		discards++
	}
	if won, err := a.WithLock(ctx, "backup-run", func(context.Context) error { return nil }); !won || err != nil {
		t.Fatalf("WithLock: won=%v err=%v", won, err)
	}
	release, err := a.Hold(ctx, "backup-run")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if discards != 2 {
		t.Fatalf("%d lock connections discarded, want 2", discards)
	}
}

func TestInstancesAreLiveWhileRegistered(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	ctx := context.Background()
	release, err := a.RegisterInstance(ctx, Instance{ID: "i-a", BootID: "boot", VolumeID: "vol", Role: "api", Started: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.RegisterInstance(ctx, Instance{ID: "i-a"}); err == nil {
		t.Fatal("a second process registered a live instance ID")
	}
	live, err := b.LiveInstances(ctx)
	if err != nil || len(live) != 1 || live[0].BootID != "boot" || live[0].VolumeID != "vol" {
		t.Fatalf("live = %+v, %v", live, err)
	}
	if ok, _ := b.InstanceLive(ctx, "i-a"); !ok {
		t.Fatal("InstanceLive false while registered")
	}
	release()
	if ok, _ := b.InstanceLive(ctx, "i-a"); ok {
		t.Fatal("InstanceLive true after release")
	}
	if live, _ := b.LiveInstances(ctx); len(live) != 0 {
		t.Fatalf("live after release = %+v", live)
	}
}

// A replica whose liveness connection drops is pruned by another's LiveInstances; once it
// holds its lock again it must be listed again, or the topology and accept-replica-config
// guards stop seeing a running replica.
func TestRelockedInstanceIsListedAgain(t *testing.T) {
	dbbackend.RequirePostgres(t)
	old := instanceRelock
	// Long enough that b sees the row pruned before a's next probe re-locks.
	instanceRelock = 2 * time.Second
	t.Cleanup(func() { instanceRelock = old })
	a, b := twoReplicas(t)
	ctx := context.Background()
	release, err := a.RegisterInstance(ctx, Instance{ID: "i-blip", BootID: "boot", VolumeID: "vol", Role: "api", Started: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND granted AND objsubid=2
 AND classid=4 AND objid=hashtext(current_schema() || ':' || 'i-blip')::oid`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminate: %d %v", n, err)
	}
	for pruned := time.Now().Add(time.Second); ; time.Sleep(20 * time.Millisecond) {
		if live, err := b.LiveInstances(ctx); err == nil && len(live) == 0 {
			break
		}
		if time.Now().After(pruned) {
			t.Fatal("the lost instance was never pruned")
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		live, err := b.LiveInstances(ctx)
		if err == nil && len(live) == 1 && live[0].ID == "i-blip" && live[0].BootID == "boot" {
			return
		}
		if time.Now().After(deadline) {
			ok, _ := b.InstanceLive(ctx, "i-blip")
			t.Fatalf("re-locked instance not listed: live=%+v err=%v lockHeld=%v", live, err, ok)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSQLiteInstanceLockRefusesMigrationWhileRunning(t *testing.T) {
	dir := t.TempDir()
	release, err := HoldInstance(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HoldInstance(dir, false)
	if err != nil {
		t.Fatalf("api and daemon share the lock: %v", err)
	}
	if _, err := HoldInstance(dir, true); !errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("exclusive while running: %v", err)
	}
	release()
	second()
	excl, err := HoldInstance(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	excl()
}

// relayedStore is a second handle on dir's global state.db whose connections pass through a
// freezable relay.
func relayedStore(t *testing.T, dir string) (*Store, *pgtest.Relay) {
	t.Helper()
	raw := os.Getenv(dbbackend.EnvTestURL)
	relay := pgtest.NewRelay(t, pgtest.Target(t, raw))
	schema, _, err := dbbackend.SchemaName(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pgdialect.NewPool(relay.URL(t, raw), 4, false)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(pgdialect.NewConnector(p, false, schema))
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = db.Close(); p.Close() })
	return &Store{baseDir: dir, db: db}, relay
}

// A frozen database ends ownership within the probe bound and bounds a new acquisition.
func TestFrozenDatabaseBoundsTheLock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	a, relay := relayedStore(t, dir)
	ctx := context.Background()
	inside := make(chan struct{})
	stopped := make(chan time.Time, 1)
	go func() {
		_, _ = a.WithLock(ctx, "role:frozen", func(owned context.Context) error {
			close(inside)
			<-owned.Done()
			stopped <- time.Now()
			return nil
		})
	}()
	<-inside
	frozenAt := time.Now()
	relay.Freeze()
	defer relay.Thaw()
	select {
	case at := <-stopped:
		if d := at.Sub(frozenAt); d > 3*time.Second {
			t.Fatalf("owner behind a frozen database cancelled after %v, want < 3s", d)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("owner behind a frozen database never cancelled")
	}
	start := time.Now()
	won, err := a.WithLock(ctx, "role:other", func(context.Context) error { return nil })
	if took := time.Since(start); took > 3*time.Second || won || !errors.Is(err, ErrLockAcquire) {
		t.Fatalf("acquisition against a frozen database: won=%v err=%v after %v", won, err, took)
	}
}

// AC25: a restore clears the old deployment's live records and keeps only refusals still in
// their window.
func TestResetReplicaStateKeepsOnlyLiveRefusals(t *testing.T) {
	dbbackend.SetURLForTest(t, "") // restore staging is always SQLite
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	for _, q := range []string{
		`INSERT INTO sessions VALUES ('h', 'u', '', '{}', 9999999999999)`,
		`INSERT INTO session_revocations VALUES ('u', 1)`,
		`INSERT INTO auth_challenges VALUES ('k', 'u', X'00', 9999999999999)`,
		`INSERT INTO security_windows VALUES ('k', 1, 9999999999999)`,
		`INSERT INTO replica_instances VALUES ('i', 'b', 'v', 'api', 1)`,
		`INSERT INTO import_jobs VALUES ('u', 'i', '{}', 1, 0, 1)`,
		`INSERT INTO spent_challenges VALUES ('live', ` + strconv.FormatInt(future, 10) + `), ('old', 1)`,
		`INSERT INTO replay_ids VALUES ('live', 'a', ` + strconv.FormatInt(future, 10) + `), ('old', 'a', 1)`,
		`INSERT INTO meta VALUES ('replica_config_fingerprint', '{}'), ('replica_volume_id', 'v'), ('sso_epoch', '3'), ('kyrecovery_key_id', 'kept')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()
	if err := ResetReplicaState(dir); err != nil {
		t.Fatal(err)
	}
	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for table, want := range map[string]int{"sessions": 0, "session_revocations": 0, "auth_challenges": 0, "security_windows": 0, "replica_instances": 0, "import_jobs": 0, "spent_challenges": 1, "replay_ids": 1} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows (%v), want %d", table, n, err, want)
		}
	}
	for _, k := range []string{"replica_config_fingerprint", "replica_volume_id", "sso_epoch"} {
		if _, err := s.Setting(k); !errors.Is(err, ErrNotFound) {
			t.Errorf("meta %s survived the restore", k)
		}
	}
	if v, _ := s.Setting("kyrecovery_key_id"); v != "kept" {
		t.Error("the restore cleared the recovery pin")
	}
}

// The refusal names a command that works: the Compose service is kypost-server.
func TestInstanceRunningNamesTheService(t *testing.T) {
	if !strings.Contains(ErrInstanceRunning.Error(), "docker compose stop kypost-server") {
		t.Fatalf("ErrInstanceRunning = %q", ErrInstanceRunning)
	}
}

// A lease has one live holder across replicas; its holder renews it, and it frees on release
// or expiry.
func TestLeaseHasOneHolder(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	take := func(s *Store, holder string, window time.Duration) bool {
		t.Helper()
		ok, err := s.TakeLease(ctx, "org-transfer:o1", holder, window)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take(a, "h1", time.Minute) || take(b, "h2", time.Minute) || !take(b, "h1", time.Minute) {
		t.Fatal("a live lease must refuse another holder and renew for its own")
	}
	if err := b.UndoReplay(ctx, "org-transfer:o1", "h2"); err != nil {
		t.Fatal(err)
	}
	if take(b, "h2", time.Minute) {
		t.Fatal("another holder's release freed the lease")
	}
	if err := a.UndoReplay(ctx, "org-transfer:o1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddDecisionUnder(&Lease{Key: "org-transfer:o1", Holder: "h1", Window: time.Minute}, Decision{MessageID: "x"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a released lease still fenced its old holder's write: %v", err)
	}
	if !take(b, "h2", time.Millisecond) {
		t.Fatal("a released lease refused")
	}
	time.Sleep(5 * time.Millisecond)
	if !take(a, "h3", time.Minute) {
		t.Fatal("an expired lease refused")
	}
}

// A decision write under a lease renews it in its own transaction: after another holder took
// the lease over, the old holder's record and decision write nothing (no check-then-write gap).
func TestLeasedWriteAfterTakeoverWritesNothing(t *testing.T) {
	a, b := twoReplicas(t)
	ctx := context.Background()
	old := &Lease{Key: "poller-message:m1", Holder: "host-a", Window: time.Millisecond}
	if ok, err := a.TakeLease(ctx, old.Key, old.Holder, old.Window); !ok || err != nil {
		t.Fatal(ok, err)
	}
	time.Sleep(5 * time.Millisecond)
	if ok, err := b.TakeLease(ctx, old.Key, "host-b", time.Minute); !ok || err != nil {
		t.Fatal("takeover of an expired lease:", ok, err)
	}
	d := Decision{MessageID: "m1", Status: "applied", Label: "Primary"}
	if err := a.RecordProcessedDecisionUnder(old, d); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("record under a lost lease: %v", err)
	}
	if err := a.AddDecisionUnder(old, d); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("decision under a lost lease: %v", err)
	}
	if seen, err := a.Seen("m1"); seen || err != nil {
		t.Fatalf("a lost lease marked the message processed: %v %v", seen, err)
	}
	if n, _ := a.DecisionsStrict(10); len(n) != 0 {
		t.Fatalf("a lost lease wrote %d decisions", len(n))
	}
	if err := b.RecordProcessedDecisionUnder(&Lease{Key: old.Key, Holder: "host-b", Window: time.Minute}, d); err != nil {
		t.Fatal("the holder's own record:", err)
	}
}
