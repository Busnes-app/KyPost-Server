package state

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableScheduledClaimsUseDatabaseClock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	db, err := volumes.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: bytes.Repeat([]byte{3}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	release, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var now int64
	if err := s.db.QueryRow(`SELECT unixepoch()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	addScheduled(t, s, "future", "u1", now+600)
	if row, ok, err := s.GetScheduledSend(t.Context(), "future"); err != nil || !ok || row.CreatedAt < now {
		t.Fatal("table creation timestamp used host value", row, ok, err)
	}
	if row, ok, err := s.ClaimScheduledSend(t.Context(), now+3600, "fast-host"); err != nil || ok {
		t.Fatal("fast host claimed future mail", row, ok, err)
	}
	if _, err := s.db.Exec(`UPDATE scheduled_sends SET send_at=unixepoch()-1,next_attempt=unixepoch()+600 WHERE id='future'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimScheduledSend(t.Context(), now+3600, "fast-host"); err != nil || ok {
		t.Fatal("fast host crossed retry deadline", ok, err)
	}
	if _, err := s.db.Exec(`UPDATE scheduled_sends SET next_attempt=unixepoch()-1 WHERE id='future'`); err != nil {
		t.Fatal(err)
	}
	other, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	var winners atomic.Int32
	for _, store := range []*Store{s, other} {
		wg.Go(func() {
			if _, ok, err := store.ClaimScheduledSend(t.Context(), 1, "slow-host"); err != nil {
				t.Error(err)
			} else if ok {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("due mail did not have exactly one claimant", winners.Load())
	}
	before := now
	if moved, err := s.DelayScheduledSend(t.Context(), "future", ScheduledSending, ScheduledWaiting, "retry", 5*time.Minute, time.Unix(1, 0)); err != nil || !moved {
		t.Fatal(moved, err)
	}
	var after, next int64
	if err := s.db.QueryRow(`SELECT unixepoch(),next_attempt FROM scheduled_sends WHERE id='future'`).Scan(&after, &next); err != nil {
		t.Fatal(err)
	}
	if next < before+300 || next > after+300 {
		t.Fatal("relative retry used host clock", before, after, next)
	}
	if moved, err := s.DelayScheduledSend(t.Context(), "future", ScheduledWaiting, ScheduledReturning, "cancelled", 2*time.Minute, time.Unix(1, 0)); err != nil || !moved {
		t.Fatal(moved, err)
	}
	if rows, err := other.ScheduledReturns(t.Context(), now+3600); err != nil || len(rows) != 0 {
		t.Fatal("fast host discovered future draft", rows, err)
	}
	if _, err := s.db.Exec(`UPDATE scheduled_sends SET next_attempt=unixepoch()-1 WHERE id='future'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := other.ScheduledReturns(t.Context(), 1); err != nil || len(rows) != 1 || rows[0].ID != "future" {
		t.Fatal("slow host hid due draft", rows, err)
	}
	current, err := s.ScheduledNow(t.Context(), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var clockAfter int64
	if err := s.db.QueryRow(`SELECT unixepoch()`).Scan(&clockAfter); err != nil {
		t.Fatal(err)
	}
	if current.Unix() < before || current.Unix() > clockAfter {
		t.Fatal("execution clock used host time", current, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := s.ScheduledNow(canceled, time.Unix(1, 0)); !errors.Is(err, context.Canceled) || !got.IsZero() {
		t.Fatal("unavailable table clock fell back", got, err)
	}
}

func TestOrdinaryScheduledDelayKeepsCallerClock(t *testing.T) {
	s := newScheduledStore(t)
	addScheduled(t, s, "ordinary", "u1", 100)
	if moved, err := s.DelayScheduledSend(t.Context(), "ordinary", ScheduledWaiting, ScheduledReturning, "cancelled", 2*time.Minute, time.Unix(100, 0)); err != nil || !moved {
		t.Fatal(moved, err)
	}
	var next int64
	if err := s.db.QueryRow(`SELECT next_attempt FROM scheduled_sends WHERE id='ordinary'`).Scan(&next); err != nil || next != 220 {
		t.Fatal("ordinary delay changed", next, err)
	}
	supplied := time.Unix(42, 123)
	if got, err := s.ScheduledNow(t.Context(), supplied); err != nil || !got.Equal(supplied) {
		t.Fatal("ordinary clock changed", got, err)
	}
}
