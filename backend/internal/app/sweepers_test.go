package app

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
)

func quietLogger(t *testing.T) *logging.Logger {
	t.Helper()
	logger, err := logging.NewWithOutput(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

// Shutdown waits for a sweeper to finish work it was doing when canceled, so
// nothing it writes can land after the process has moved on.
func TestRunSweepersWaitsForInFlightWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var finished atomic.Bool
	wait := runSweepers(ctx, quietLogger(t), time.Minute, func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // work that outlives the cancel
		finished.Store(true)
	})
	cancel()
	wait()
	if !finished.Load() {
		t.Fatal("wait returned before the sweeper finished")
	}
}

// A sweeper that never returns cannot hold shutdown past the drain bound.
func TestRunSweepersDrainIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stuck := make(chan struct{})
	defer close(stuck)
	wait := runSweepers(ctx, quietLogger(t), 50*time.Millisecond, func(context.Context) { <-stuck })
	cancel()
	start := time.Now()
	wait()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("wait took %v despite a 50ms drain bound", took)
	}
}
