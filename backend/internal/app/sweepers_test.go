package app

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/health"
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

// shortenHealthCheck makes monitorHealth restart after one second unhealthy.
func shortenHealthCheck(t *testing.T) {
	t.Setenv("UNHEALTHY_RESTART_SECONDS", "1")
	prev := healthCheckInterval
	healthCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { healthCheckInterval = prev })
}

// A health-triggered restart drains an in-flight sweeper, as SIGTERM does,
// instead of exiting under it.
func TestHealthRestartWaitsForInFlightSweeper(t *testing.T) {
	shortenHealthCheck(t)
	svc := health.NewService()
	svc.MarkUnhealthy("imap unreachable for all users")

	ctx, restart := context.WithCancelCause(context.Background())
	defer restart(nil)
	var finished atomic.Bool
	wait := runSweepers(ctx, quietLogger(t), time.Minute, func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a claimed scheduled send finishing
		finished.Store(true)
	})
	go monitorHealth(ctx, quietLogger(t), svc, healthCheckInterval, restart)

	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("monitorHealth never triggered shutdown")
	}
	wait()
	if !finished.Load() {
		t.Fatal("shutdown returned before the sweeper finished")
	}
	if err := healthRestart(ctx); !errors.Is(err, errUnhealthy) {
		t.Fatalf("healthRestart = %v, want errUnhealthy", err)
	}
}
