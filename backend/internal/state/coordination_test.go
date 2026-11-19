package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	c, err := pgdialect.NewConnector(dbbackend.WithSearchPath(relay.URL(t, raw), schema), false)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
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
		`INSERT INTO import_jobs VALUES ('u', 'i', '{}', 0, 1)`,
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
