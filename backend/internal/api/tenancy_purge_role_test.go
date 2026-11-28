package api

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/worker"
)

// An org purge whose owner loses the role mid-purge (its lock session ends, as when the
// role flapped in the KyCalendar #244 e2e run) stops there, and the next owner finishes it:
// the purge is never skipped. Real elections on PostgreSQL, the production timings.
func TestOrgPurgeInterruptedByRoleSwitchFinishesOnNextOwner(t *testing.T) {
	dbbackend.RequirePostgres(t)
	px := newPurgeFixture(t)
	setOrg(t, px.db, orgA, "deleted", false, `{}`)
	dir := t.TempDir()
	a, err := state.New(dir)
	check(px.orgFixture, err)
	b, err := state.New(dir)
	check(px.orgFixture, err)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	schema, _, err := dbbackend.SchemaName(filepath.Join(dir, "state.db"))
	check(px.orgFixture, err)
	const role = "purge-test"

	var aOwned context.Context
	var killed atomic.Bool
	purgeStepHook = func(step int) error {
		if step != 3 || !killed.CompareAndSwap(false, true) {
			return nil
		}
		var n int
		if err := px.db.QueryRow(`SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory'
 AND granted AND classid = 3 AND objsubid = 2 AND objid = hashtext(?)::oid`, schema+":role:"+role).Scan(&n); err != nil || n != 1 {
			t.Errorf("terminate the owner's lock session: %d %v", n, err)
		}
		<-aOwned.Done()
		return context.Cause(aOwned)
	}
	defer func() { purgeStepHook = nil }()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	opts := worker.Options{Retry: 50 * time.Millisecond, Grace: state.ProbeBound() + time.Second}
	interrupted := make(chan error, 1)
	finished := make(chan struct{})
	var wg sync.WaitGroup
	// a runs on its own context and is stopped before b starts, so b is sure to win the role.
	aCtx, stopA := context.WithCancel(ctx)
	defer stopA()
	var aDone sync.WaitGroup
	aDone.Go(func() {
		worker.Run(aCtx, a, role, opts, []worker.Job{{Name: "purge", Run: func(owned context.Context) {
			if aOwned != nil {
				<-owned.Done() // a re-elected before it is stopped: no second purge
				return
			}
			aOwned = owned
			interrupted <- px.srv.runPurges(owned, time.Now())
		}}})
	})
	var err1 error
	select {
	case err1 = <-interrupted:
	case <-time.After(30 * time.Second):
		t.Fatal("the first owner never ran the purge")
	}
	if !errors.Is(err1, state.ErrOwnershipLost) {
		t.Fatalf("the interrupted purge returned %v, want ownership lost", err1)
	}
	stopA()
	aDone.Wait()
	wg.Go(func() {
		worker.Run(ctx, b, role, opts, []worker.Job{{Name: "purge", Run: func(owned context.Context) {
			for owned.Err() == nil {
				if err := px.srv.runPurges(owned, time.Now()); err == nil {
					close(finished)
					<-owned.Done()
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}}})
	})
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the next owner never finished the purge")
	}
	stop()
	wg.Wait()
	px.assertPurged(t)
}
