package app

import (
	"context"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// A role's new owner on PostgreSQL waits out the lock probe before its first job: until then
// the old owner may not yet have noticed it lost the lock, and both would run the role.
// worker's own test proves the wait with its grace; this pins the grace production uses.
func TestElectionGraceOutlivesTheProbe(t *testing.T) {
	o := electionOptions()
	if !dbbackend.Postgres() {
		if o.Grace != 0 {
			t.Fatalf("SQLite grace %v: a flock cannot be lost, so a new owner need not wait", o.Grace)
		}
		return
	}
	if o.Grace <= state.ProbeBound() {
		t.Fatalf("election grace %v does not outlive the lock probe bound %v", o.Grace, state.ProbeBound())
	}
}

// With native mail off the mail-runtime role has no work, so it is never elected: an owner
// returning at once re-elected the role every few seconds (fsn1). The nil store makes an
// election panic.
func TestMailRuntimeNotElectedWithoutNativeMail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	wait := electMailRuntime(ctx, runDeps{nativeMail: false})
	<-ctx.Done()
	wait()
}
