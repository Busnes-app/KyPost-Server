package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

func shortFenceWait(t *testing.T, d time.Duration) {
	old := fenceWait
	fenceWait = d
	t.Cleanup(func() { fenceWait = old })
}

func collide(t *testing.T, pairs map[string]string) {
	t.Cleanup(func() { fenceName = func(name string) string { return name } })
	fenceName = func(name string) string {
		if to, ok := pairs[name]; ok {
			return to
		}
		return name
	}
}

// fenceLocks counts the sessions holding (granted) or waiting for name's advisory key.
func fenceLocks(t *testing.T, s *Store, name string, granted bool) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 1
 AND granted = $2 AND ((classid::bigint << 32) | objid::bigint) = `+fenceKey, fenceName(name), granted).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

// hold enters name's fence on s and stays until leave closes.
func hold(s *Store, fences []Fence) (inside chan struct{}, leave chan struct{}, done chan error) {
	inside, leave, done = make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithFences(context.Background(), fences, func(context.Context) error {
			close(inside)
			<-leave
			return nil
		})
	}()
	return
}

func nop(context.Context) error { return nil }

func TestFenceSetsRefuseBadNames(t *testing.T) {
	dbbackend.RequirePostgres(t)
	s, _ := twoReplicas(t)
	for _, set := range [][]Fence{
		nil,
		{{Name: "native-domains"}},
		{{Name: "fence:unknown"}},
		{{Name: "fence:settings"}},
		{{Name: "fence:settings:"}},
		{{Name: "fence:native-domains:x"}},
		{{Name: FenceSettings + strings.Repeat("u", 400)}},
		{{Name: FenceSSO}, {Name: FenceSSO, Shared: true}},
	} {
		if err := s.WithFences(context.Background(), set, nop); !errors.Is(err, errFence) {
			t.Errorf("%v: %v, want errFence", set, err)
		}
	}
}

func TestFencesNeedPostgres(t *testing.T) {
	dbbackend.SkipOnPostgres(t, "the SQLite refusal")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.WithFences(context.Background(), []Fence{{Name: FenceSSO}}, nop); !errors.Is(err, errFencePG) {
		t.Fatal(err)
	}
}

// Shared holders coexist across replicas; an exclusive one waits for them, and is refused
// at the bound naming only the kind.
func TestFenceExclusiveAgainstShared(t *testing.T) {
	dbbackend.RequirePostgres(t)
	shortFenceWait(t, 500*time.Millisecond)
	a, b := twoReplicas(t)
	name := FenceSettings + "user-7f3a"
	inside, leave, done := hold(a, []Fence{{Name: name, Shared: true}})
	<-inside
	if err := b.WithFences(context.Background(), []Fence{{Name: name, Shared: true}}, nop); err != nil {
		t.Fatalf("a second shared holder: %v", err)
	}
	start := time.Now()
	err := b.WithFences(context.Background(), []Fence{{Name: name}}, nop)
	if !errors.Is(err, ErrFenceWait) || strings.Contains(err.Error(), "user-7f3a") || !strings.HasSuffix(err.Error(), ": settings") {
		t.Fatalf("exclusive against shared: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the bounded wait took %v", took)
	}
	close(leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := b.WithFences(context.Background(), []Fence{{Name: name}}, nop); err != nil {
		t.Fatalf("exclusive after the readers left: %v", err)
	}
	// And the converse: a reader waits for a writer.
	inside, leave, done = hold(a, []Fence{{Name: name}})
	<-inside
	if err := b.WithFences(context.Background(), []Fence{{Name: name, Shared: true}}, nop); !errors.Is(err, ErrFenceWait) {
		t.Fatalf("shared against exclusive: %v", err)
	}
	close(leave)
	<-done
}

// MH's unverified claim, tested: PostgreSQL queues a new shared request behind a waiting
// exclusive one, so a stream of readers cannot starve a writer (fsutil.LockFileGated's job).
func TestFenceWriterNotStarvedByReaders(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	name := FenceNativeDomains
	r1in, r1leave, r1done := hold(a, []Fence{{Name: name, Shared: true}})
	<-r1in
	var mu sync.Mutex
	var order []string
	enter := func(who string) {
		mu.Lock()
		order = append(order, who)
		mu.Unlock()
	}
	wleave := make(chan struct{})
	wdone := make(chan error, 1)
	go func() {
		wdone <- b.WithFences(context.Background(), []Fence{{Name: name}}, func(context.Context) error {
			enter("writer")
			<-wleave
			return nil
		})
	}()
	eventually(t, "the writer to queue", func() bool { return fenceLocks(t, a, name, false) == 1 })
	// A stream of readers arrives while the first still holds the fence.
	var readers sync.WaitGroup
	for i := range 5 {
		s := a
		if i%2 == 1 {
			s = b
		}
		readers.Go(func() {
			if err := s.WithFences(context.Background(), []Fence{{Name: name, Shared: true}}, func(context.Context) error {
				enter("reader")
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	eventually(t, "the readers to queue behind the writer", func() bool { return fenceLocks(t, a, name, false) == 6 })
	mu.Lock()
	if len(order) != 0 {
		t.Fatalf("entered while the first reader held the fence: %v", order)
	}
	mu.Unlock()
	close(r1leave)
	if err := <-r1done; err != nil {
		t.Fatal(err)
	}
	eventually(t, "the writer to enter", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) > 0 })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if len(order) != 1 || order[0] != "writer" {
		t.Fatalf("order %v: a later reader overtook the waiting writer", order)
	}
	mu.Unlock()
	close(wleave)
	if err := <-wdone; err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if len(order) != 6 {
		t.Fatalf("order %v", order)
	}
}

// Killing the fence's session mid-section cancels the holder within the probe bound, and a
// second replica wins the fence only after that loss.
func TestFenceLossCancelsTheHolderAndOnlyThenAdmitsAnother(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, b := twoReplicas(t)
	name := FenceQuota + "mbx-1"
	ainside := make(chan struct{})
	cancelled := make(chan time.Time, 1)
	go func() {
		_ = a.WithFences(context.Background(), []Fence{{Name: FenceSettings + "u1"}, {Name: name}}, func(owned context.Context) error {
			close(ainside)
			<-owned.Done()
			if !errors.Is(context.Cause(owned), ErrOwnershipLost) {
				t.Errorf("cause = %v", context.Cause(owned))
			}
			cancelled <- time.Now()
			return context.Cause(owned)
		})
	}()
	<-ainside
	won := make(chan time.Time, 1)
	go func() {
		if err := b.WithFences(context.Background(), []Fence{{Name: name}}, func(context.Context) error {
			won <- time.Now()
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	eventually(t, "replica b to queue", func() bool { return fenceLocks(t, b, name, false) == 1 })
	select {
	case <-won:
		t.Fatal("replica b entered while a held the fence")
	case <-time.After(1500 * time.Millisecond):
	}
	killed := time.Now()
	var n int
	if err := b.db.QueryRowContext(context.Background(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory'
 AND objsubid = 1 AND granted AND ((classid::bigint << 32) | objid::bigint) = `+fenceKey, name).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminate: %d %v", n, err)
	}
	var lost time.Time
	select {
	case lost = <-cancelled:
		if d := lost.Sub(killed); d > ProbeBound()+time.Second {
			t.Fatalf("holder cancelled %v after its session ended", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder never cancelled")
	}
	select {
	case at := <-won:
		if at.Before(killed) {
			t.Fatal("replica b won before the loss")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replica b never won after the loss")
	}
}

// A forced key collision inside one set is harmless (one session, re-entrant, whatever the
// modes); across two requests it waits, and the bound ends it.
func TestFenceCollisions(t *testing.T) {
	dbbackend.RequirePostgres(t)
	shortFenceWait(t, 700*time.Millisecond)
	a, b := twoReplicas(t)
	collide(t, map[string]string{FenceQuota + "m2": FenceSettings + "u1", FenceSettings + "u3": FenceSettings + "u1"})
	set := []Fence{{Name: FenceSettings + "u1"}, {Name: FenceQuota + "m2", Shared: true}, {Name: FenceSettings + "u3"}}
	if err := a.WithFences(context.Background(), set, nop); err != nil {
		t.Fatalf("colliding fences in one set: %v", err)
	}
	inside, leave, done := hold(a, []Fence{{Name: FenceSettings + "u1"}})
	<-inside
	if err := b.WithFences(context.Background(), []Fence{{Name: FenceQuota + "m2"}}, nop); !errors.Is(err, ErrFenceWait) {
		t.Fatalf("a colliding fence on another request: %v", err)
	}
	close(leave)
	<-done
}

// Two requests that each hold a fence and then ask, on a second session, for one colliding
// with the other's: a cycle PostgreSQL cannot see. The bound ends it in ErrFenceWait.
func TestFenceCycleEndsInABoundedWait(t *testing.T) {
	dbbackend.RequirePostgres(t)
	shortFenceWait(t, 700*time.Millisecond)
	a, b := twoReplicas(t)
	collide(t, map[string]string{FenceSettings + "a2": FenceSettings + "a", FenceSettings + "b2": FenceSettings + "b"})
	var both sync.WaitGroup
	both.Add(2)
	run := func(s *Store, mine, theirs string) chan error {
		out := make(chan error, 1)
		go func() {
			var inner error
			err := s.WithFences(context.Background(), []Fence{{Name: FenceSettings + mine}}, func(context.Context) error {
				both.Done()
				both.Wait()
				inner = s.WithFences(context.Background(), []Fence{{Name: FenceSettings + theirs}}, nop)
				return nil
			})
			out <- errors.Join(err, inner)
		}()
		return out
	}
	start := time.Now()
	ra, rb := run(a, "a", "b2"), run(b, "b", "a2")
	ea, eb := <-ra, <-rb
	if !errors.Is(ea, ErrFenceWait) && !errors.Is(eb, ErrFenceWait) {
		t.Fatalf("a cycle ended without a bounded wait: %v, %v", ea, eb)
	}
	if took := time.Since(start); took > 4*fenceWait {
		t.Fatalf("the cycle took %v", took)
	}
}

// Fence sessions come from one capped pool per schema in the process, shared by every Store:
// once it is full, another set on any Store is a bounded wait, and the store's own queries
// still run.
func TestFenceSessionsDoNotStarveTheStore(t *testing.T) {
	dbbackend.RequirePostgres(t)
	shortFenceWait(t, 500*time.Millisecond)
	a, b := twoReplicas(t)
	var leaves []chan struct{}
	for i := range maxFenceSessions {
		inside, leave, _ := hold(a, []Fence{{Name: FenceMailbox + "m" + strings.Repeat("x", i+1)}})
		<-inside
		leaves = append(leaves, leave)
	}
	defer func() {
		for _, l := range leaves {
			close(l)
		}
	}()
	for _, s := range []*Store{a, b} {
		if err := s.WithFences(context.Background(), []Fence{{Name: FenceSSO}}, nop); !errors.Is(err, ErrFenceWait) {
			t.Fatalf("a set past the process's session cap: %v", err)
		}
	}
	if _, _, _, err := a.TakeAttempt(context.Background(), "fence-test", 1, time.Minute); err != nil {
		t.Fatalf("the store's own query with every fence session held: %v", err)
	}
}
