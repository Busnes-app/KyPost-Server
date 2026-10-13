package state

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func newScheduledStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addScheduled(t *testing.T, s *Store, id, user string, sendAt int64) {
	t.Helper()
	if err := s.AddScheduledSend(context.Background(), ScheduledSend{ID: id, UserID: user, Mailbox: user, SendAt: sendAt, CreatedAt: 1, Sealed: []byte("job"), Summary: []byte("sum")}, 50, 1<<30); err != nil {
		t.Fatal(err)
	}
}

// Two stores on one file stand in for two processes; only one claim may win.
func TestScheduledClaimIsExclusive(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	addScheduled(t, a, "row", "u1", 100)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			if _, ok, err := store.ClaimScheduledSend(context.Background(), 100); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}([]*Store{a, b}[i%2])
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims won, want 1", wins.Load())
	}
}

func TestScheduledClaimWaitsForSendTimeAndRetry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	addScheduled(t, s, "row", "u1", 100)
	if _, ok, _ := s.ClaimScheduledSend(ctx, 99); ok {
		t.Fatal("claimed before send time")
	}
	row, ok, err := s.ClaimScheduledSend(ctx, 100)
	if err != nil || !ok || string(row.Sealed) != "job" {
		t.Fatal(row, ok, err)
	}
	if moved, _ := s.MoveScheduledSend(ctx, "row", ScheduledSending, ScheduledWaiting, "", 400); !moved {
		t.Fatal("retry not recorded")
	}
	if _, ok, _ := s.ClaimScheduledSend(ctx, 399); ok {
		t.Fatal("claimed before the retry time")
	}
	if _, ok, _ := s.ClaimScheduledSend(ctx, 400); !ok {
		t.Fatal("retry not claimed")
	}
	if moved, _ := s.MoveScheduledSend(ctx, "row", ScheduledWaiting, "returning", "late", 0); moved {
		t.Fatal("moved from a status the row is not in")
	}
	if moved, _ := s.MoveScheduledSend(ctx, "row", ScheduledSending, "sent", "", 0); !moved {
		t.Fatal("not finished")
	}
	got, _, _ := s.GetScheduledSend(ctx, "row")
	if got.Status != "sent" || got.Sealed != nil {
		t.Fatalf("final row kept its job: %+v", got)
	}
}

func TestScheduledInterruptedSendsBecomeUncertain(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	addScheduled(t, s, "sending", "u1", 100)
	addScheduled(t, s, "waiting", "u1", 900)
	if _, ok, _ := s.ClaimScheduledSend(ctx, 100); !ok {
		t.Fatal("no claim")
	}
	if err := s.InterruptScheduledSends(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetScheduledSend(ctx, "sending")
	if got.Status != "uncertain" || got.Sealed != nil {
		t.Fatalf("interrupted send: %+v", got)
	}
	if _, ok, _ := s.ClaimScheduledSend(ctx, 1000); !ok {
		t.Fatal("waiting row lost")
	}
	if _, ok, _ := s.ClaimScheduledSend(ctx, 1000); ok {
		t.Fatal("uncertain row claimed again")
	}
}

func TestScheduledPendingCapIsPerUser(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := range 2 {
		if err := s.AddScheduledSend(ctx, ScheduledSend{ID: strconv.Itoa(i), UserID: "u1", Mailbox: "u1", SendAt: 100, Summary: []byte{}}, 2, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddScheduledSend(ctx, ScheduledSend{ID: "x", UserID: "u1", Mailbox: "u1", SendAt: 100, Summary: []byte{}}, 2, 1<<30); !errors.Is(err, ErrScheduledFull) {
		t.Fatal("cap not enforced", err)
	}
	if err := s.AddScheduledSend(ctx, ScheduledSend{ID: "y", UserID: "u2", Mailbox: "u2", SendAt: 100, Summary: []byte{}}, 2, 1<<30); err != nil {
		t.Fatal("another user's cap shared", err)
	}
	if moved, _ := s.MoveScheduledSend(ctx, "0", ScheduledWaiting, "returned", "", 0); !moved {
		t.Fatal("not moved")
	}
	if err := s.AddScheduledSend(ctx, ScheduledSend{ID: "x", UserID: "u1", Mailbox: "u1", SendAt: 100, Summary: []byte{}}, 2, 1<<30); err != nil {
		t.Fatal("a final row still counted", err)
	}
}

func TestFenceRestoredScheduledSends(t *testing.T) {
	if err := FenceRestoredScheduledSends(t.TempDir()); err != nil {
		t.Fatal("a capsule without state.db refused", err)
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	addScheduled(t, s, "waiting", "u1", 900)
	addScheduled(t, s, "sending", "u1", 100)
	if _, ok, _ := s.ClaimScheduledSend(ctx, 100); !ok {
		t.Fatal("no claim")
	}
	_ = s.Close()
	if err := FenceRestoredScheduledSends(dir); err != nil {
		t.Fatal(err)
	}
	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waiting, _, _ := s.GetScheduledSend(ctx, "waiting")
	sending, _, _ := s.GetScheduledSend(ctx, "sending")
	if waiting.Status != ScheduledReturning || waiting.Reason != ScheduledRestored || string(waiting.Sealed) != "job" || sending.Status != "uncertain" {
		t.Fatalf("restored rows: %+v %+v", waiting, sending)
	}
	if returns, _ := s.ScheduledReturns(ctx, 0); len(returns) != 1 {
		t.Fatal("restored row not due for its draft", returns)
	}
}

// One user's slow relay must hold one worker at most: while a user has a row
// sending, the claim skips their other rows. Fresh rows go before retries.
func TestClaimScheduledSendIsFairAcrossUsers(t *testing.T) {
	s := newScheduledStore(t)
	ctx := context.Background()
	for _, r := range []struct{ id, user string }{{"a1", "a"}, {"a2", "a"}, {"b1", "b"}} {
		if err := s.AddScheduledSend(ctx, ScheduledSend{ID: r.id, UserID: r.user, Mailbox: r.user, SendAt: 100, Sealed: []byte("j"), Summary: []byte{}}, 50, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	// a2 failed once and waits to retry; a1 is fresh.
	if _, err := s.db.ExecContext(ctx, `UPDATE scheduled_sends SET next_attempt=50 WHERE id='a2'`); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.ClaimScheduledSend(ctx, 200)
	if err != nil || !ok || first.ID == "a2" {
		t.Fatalf("first claim = %+v, %v, %v; a retry must not go before fresh rows", first, ok, err)
	}
	second, ok, err := s.ClaimScheduledSend(ctx, 200)
	if err != nil || !ok || second.UserID == first.UserID {
		t.Fatalf("second claim = %+v, %v, %v; want the other user while %s is sending", second, ok, err, first.UserID)
	}
	if third, ok, _ := s.ClaimScheduledSend(ctx, 200); ok {
		t.Fatalf("third claim = %+v; both users already have a row sending", third)
	}
}

// The byte budget is enforced in the same insert as the count.
func TestAddScheduledSendEnforcesByteBudget(t *testing.T) {
	s := newScheduledStore(t)
	ctx := context.Background()
	add := func(id, user string, n int) error {
		return s.AddScheduledSend(ctx, ScheduledSend{ID: id, UserID: user, Mailbox: user, SendAt: 100, Sealed: make([]byte, n), Summary: []byte{}}, 50, 100)
	}
	if err := add("1", "u", 60); err != nil {
		t.Fatal(err)
	}
	if err := add("2", "u", 41); !errors.Is(err, ErrScheduledFull) {
		t.Fatalf("over budget: %v, want ErrScheduledFull", err)
	}
	if err := add("3", "u", 40); err != nil {
		t.Fatalf("exactly at budget: %v", err)
	}
	if err := add("4", "other", 100); err != nil {
		t.Fatalf("another user's budget is separate: %v", err)
	}
}
