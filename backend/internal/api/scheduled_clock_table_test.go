package api

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestTableScheduledSweepIgnoresReplicaClock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	f := newScheduledFixture(t, "451 temporary test refusal")
	id := f.schedule(t, sendTime(), nil)
	mountServerDocuments(t, f.srv)
	db, err := dbbackend.OpenFile(filepath.Join(f.srv.stateDir, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before int64
	if err := db.QueryRow(`SELECT unixepoch()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fast := clockAt(time.Unix(before+365*24*3600, 0))
	f.srv.sweepScheduledSends(context.Background(), fast)
	if dials, _ := f.smtp.counts(); dials != 0 || f.row(t, id).Status != state.ScheduledWaiting || len(f.fake.savedDrafts) != 0 {
		t.Fatal("fast sweep sent or returned future mail", dials, f.row(t, id))
	}
	// Keep the frozen delivery intact; move only the stopped fixture's due time.
	if _, err := db.Exec(`UPDATE scheduled_sends SET send_at=unixepoch()-1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	f.srv.sweepScheduledSends(context.Background(), fast)
	if dials, _ := f.smtp.counts(); dials != 1 || f.row(t, id).Status != state.ScheduledWaiting || len(f.fake.savedDrafts) != 0 {
		t.Fatal("fast clock changed execution deadline", dials, f.row(t, id))
	}
	var after, next int64
	if err := db.QueryRow(`SELECT unixepoch(),next_attempt FROM scheduled_sends WHERE id=?`, id).Scan(&after, &next); err != nil {
		t.Fatal(err)
	}
	if next < before+300 || next > after+300 {
		t.Fatal("retry deadline used host clock", before, after, next)
	}
	f.srv.sweepScheduledSends(context.Background(), fast)
	if dials, _ := f.smtp.counts(); dials != 1 {
		t.Fatal("fast sweep bypassed retry delay", dials)
	}
	if _, err := db.Exec(`UPDATE scheduled_sends SET next_attempt=unixepoch()-1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(time.Unix(1, 0)))
	if dials, _ := f.smtp.counts(); dials != 2 || f.row(t, id).Status != state.ScheduledWaiting {
		t.Fatal("slow sweep hid due retry", dials, f.row(t, id))
	}
}

func TestTableScheduledClockFailurePreservesHeldReason(t *testing.T) {
	dbbackend.RequirePostgres(t)
	for _, reason := range []string{reasonQuota, reasonHeld} {
		for cancelAt := 1; cancelAt <= 2; cancelAt++ {
			t.Run(fmt.Sprintf("%s/clock-%d", reason, cancelAt), func(t *testing.T) {
				f := newScheduledFixture(t, "250 queued")
				id := f.schedule(t, sendTime(), nil)
				mountServerDocuments(t, f.srv)
				db, err := dbbackend.OpenFile(filepath.Join(f.srv.stateDir, "state.db"), "")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`UPDATE scheduled_sends SET send_at=unixepoch()-1,reason=? WHERE id=?`, reason, id); err != nil {
					t.Fatal(err)
				}
				row, ok, err := f.srv.globalStore.ClaimScheduledSend(t.Context(), 0, "clock-test")
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				key, err := f.srv.scheduledSendKey()
				if err != nil {
					t.Fatal(err)
				}
				var before int64
				if err := db.QueryRow(`SELECT unixepoch()`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				now := func() time.Time {
					calls++
					if calls == cancelAt {
						cancel()
					}
					return time.Now()
				}
				f.srv.runScheduled(ctx, key, row, now)
				if calls < cancelAt {
					t.Fatal("clock failure injection was not reached", calls, cancelAt)
				}
				got := f.row(t, id)
				if got.Status != state.ScheduledWaiting || got.Reason != reason || !bytes.Equal(got.Sealed, row.Sealed) || !bytes.Equal(got.Summary, row.Summary) {
					t.Fatal("clock failure lost retained retry reason or sealed mail", got.Status, got.Reason, len(got.Sealed))
				}
				if dials, _ := f.smtp.counts(); dials != 0 || len(f.fake.savedDrafts) != 0 {
					t.Fatal("clock failure submitted mail or returned draft", dials, len(f.fake.savedDrafts))
				}
				var next, dbNow int64
				if err := db.QueryRow(`SELECT next_attempt,unixepoch() FROM scheduled_sends WHERE id=?`, id).Scan(&next, &dbNow); err != nil || next < before+300 || next > dbNow+300 {
					t.Fatal("clock failure did not retain bounded database backoff", next, dbNow, err)
				}
			})
		}
	}
}
