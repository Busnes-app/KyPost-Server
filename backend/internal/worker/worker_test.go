package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Three servers on one database and volume: each role runs on exactly one of them.
func TestOneOfThreeServersRunsEachRole(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	roles := []string{RoleDaemon, RoleAPISweepers, RoleMailRuntime}
	var ticks [3][3]atomic.Int64 // [server][role]
	for srv := range 3 {
		s, err := state.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		for r, role := range roles {
			wg.Go(func() {
				Run(ctx, s, role, Options{Retry: 20 * time.Millisecond}, []Job{{Name: "tick", Every: 10 * time.Millisecond, Run: func(context.Context) { ticks[srv][r].Add(1) }}})
			})
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for r := range roles {
		for {
			running := 0
			for srv := range 3 {
				if ticks[srv][r].Load() > 0 {
					running++
				}
			}
			if running == 1 {
				break
			}
			if running > 1 || time.Now().After(deadline) {
				t.Fatalf("role %s ran on %d servers", roles[r], running)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Let every standby retry many times; the counts must still come from one server.
	time.Sleep(300 * time.Millisecond)
	for r, role := range roles {
		running := 0
		for srv := range 3 {
			if ticks[srv][r].Load() > 0 {
				running++
			}
		}
		if running != 1 {
			t.Fatalf("role %s ran on %d servers", role, running)
		}
	}
}

// fakeLocker hands the lock to the next caller the moment the database would drop the old
// session, while the old owner only notices after probeBound, as a real frozen probe does.
type fakeLocker struct {
	mu         sync.Mutex
	held       bool
	probeBound time.Duration
	lose       chan struct{} // closed by the test: the database drops the owner's session
}

func (f *fakeLocker) WithLock(ctx context.Context, _ string, work func(context.Context) error) (bool, error) {
	f.mu.Lock()
	if f.held {
		f.mu.Unlock()
		return false, nil
	}
	f.held = true
	lose := f.lose
	f.mu.Unlock()
	owned, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-lose:
			f.mu.Lock()
			f.held = false // the database already lets someone else in
			f.lose = make(chan struct{})
			f.mu.Unlock()
			time.Sleep(f.probeBound) // the owner's probe has not noticed yet
			cancel(state.ErrOwnershipLost)
		case <-owned.Done():
		}
	}()
	err := work(owned)
	cancel(nil)
	return true, err
}

// The new owner's first job starts only after the predecessor's job has stopped. With
// Grace 0 the jobs overlap and this fails.
func TestNewOwnerStartsAfterThePredecessorStops(t *testing.T) {
	testGrace(t, 600*time.Millisecond)
}

func testGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	const probe = 400 * time.Millisecond
	locks := &fakeLocker{probeBound: probe, lose: make(chan struct{})}
	var mu sync.Mutex
	var running, overlaps int
	job := Job{Name: "deposit", Every: time.Hour, Run: func(ctx context.Context) {
		mu.Lock()
		running++
		if running > 1 {
			overlaps++
		}
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		running--
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	opts := Options{Retry: 10 * time.Millisecond, Grace: grace}
	started := make(chan struct{})
	var once sync.Once
	first := job
	first.Run = func(ctx context.Context) { once.Do(func() { close(started) }); job.Run(ctx) }
	wg.Go(func() { Run(ctx, locks, RoleDaemon, opts, []Job{first}) })
	<-started
	wg.Go(func() { Run(ctx, locks, RoleDaemon, opts, []Job{job}) })
	locks.mu.Lock()
	close(locks.lose)
	locks.mu.Unlock()
	time.Sleep(probe + grace + 300*time.Millisecond)
	cancel()
	wg.Wait()
	if overlaps != 0 {
		t.Fatalf("grace %v: the new owner ran beside the old one %d times", grace, overlaps)
	}
}

// On PostgreSQL, ending the owner's session hands the role to another server.
func TestTakeoverAfterTheOwnerSessionEnds(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	a, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancelA(); cancelB(); wg.Wait() }()
	var ranA, ranB atomic.Int64
	opts := Options{Retry: 200 * time.Millisecond, Grace: state.ProbeBound() + time.Second}
	wg.Go(func() {
		Run(ctxA, a, RoleDaemon, opts, []Job{{Name: "a", Every: 50 * time.Millisecond, Run: func(context.Context) { ranA.Add(1) }}})
	})
	for ranA.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	wg.Go(func() {
		Run(ctxB, b, RoleDaemon, opts, []Job{{Name: "b", Every: 50 * time.Millisecond, Run: func(context.Context) { ranB.Add(1) }}})
	})
	time.Sleep(time.Second)
	if ranB.Load() != 0 {
		t.Fatal("standby ran while the owner held the role")
	}
	cancelA() // the owner shuts down: its lock is released first
	deadline := time.Now().Add(2*opts.Retry + opts.Grace + 2*time.Second)
	for ranB.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("standby never took the role over")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
