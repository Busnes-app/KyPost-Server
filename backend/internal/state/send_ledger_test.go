package state

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmitSendHourlyMessageLimit(t *testing.T) {
	s, ctx := newScheduledStore(t), context.Background()
	const now = int64(1_000_000)
	for i := range 3 {
		if _, err := s.AdmitSend(ctx, "m"+strconv.Itoa(i), "u", 1, 3, 100, now+int64(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	wait, err := s.AdmitSend(ctx, "m3", "u", 1, 3, 100, now+10)
	if !errors.Is(err, ErrSendQuota) {
		t.Fatalf("fourth send in the hour = %v, want ErrSendQuota", err)
	}
	// The first send leaves the hour at now+3600, 3590s after the attempt.
	if wait != 3590*time.Second {
		t.Fatalf("wait = %v, want 3590s", wait)
	}
	if _, err := s.AdmitSend(ctx, "m3", "u", 1, 3, 100, now+3600); err != nil {
		t.Fatalf("send after the first aged out: %v", err)
	}
	if _, err := s.AdmitSend(ctx, "other", "v", 1, 3, 100, now+10); err != nil {
		t.Fatalf("another user's quota was spent: %v", err)
	}
}

func TestAdmitSendDailyRecipientLimit(t *testing.T) {
	s, ctx := newScheduledStore(t), context.Background()
	const now = int64(1_000_000)
	if _, err := s.AdmitSend(ctx, "a", "u", 6, 100, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitSend(ctx, "b", "u", 3, 100, 10, now+7200); err != nil {
		t.Fatal(err)
	}
	wait, err := s.AdmitSend(ctx, "c", "u", 4, 100, 10, now+7300)
	if !errors.Is(err, ErrSendQuota) {
		t.Fatalf("over the day = %v, want ErrSendQuota", err)
	}
	// Freeing 3 recipients needs only the first send to age out.
	if want := time.Duration(now+sendDay-(now+7300)) * time.Second; wait != want {
		t.Fatalf("wait = %v, want %v", wait, want)
	}
	if _, err := s.AdmitSend(ctx, "c", "u", 1, 100, 10, now+7300); err != nil {
		t.Fatalf("a send that fits was refused: %v", err)
	}
	if _, err := s.AdmitSend(ctx, "d", "u", 11, 100, 10, now+7300); !errors.Is(err, ErrSendTooLarge) {
		t.Fatalf("more recipients than a day allows = %v, want ErrSendTooLarge", err)
	}
}

// A retried scheduled send keeps its id and is not charged twice.
func TestAdmitSendIsIdempotentPerID(t *testing.T) {
	s, ctx := newScheduledStore(t), context.Background()
	for range 3 {
		if _, err := s.AdmitSend(ctx, "job", "u", 5, 1, 5, 1_000_000); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AdmitSend(ctx, "next", "u", 1, 1, 5, 1_000_001); !errors.Is(err, ErrSendQuota) {
		t.Fatalf("second message = %v, want ErrSendQuota", err)
	}
}

// Two stores on one file stand in for the API and daemon processes; racing
// senders must not overshoot the quota.
func TestAdmitSendRaceAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	stores := make([]*Store, 2)
	for i := range stores {
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		stores[i] = s
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			_, err := stores[i%2].AdmitSend(context.Background(), "m"+strconv.Itoa(i), "u", 1, 7, 100, 1_000_000)
			switch {
			case err == nil:
				admitted.Add(1)
			case !errors.Is(err, ErrSendQuota):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := admitted.Load(); got != 7 {
		t.Fatalf("admitted %d sends, want exactly 7", got)
	}
}
