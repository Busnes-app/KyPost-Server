package api

import (
	"context"
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
