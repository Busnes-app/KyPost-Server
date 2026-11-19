// Package worker runs a role's background jobs on one process at a time.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Roles. Each is elected separately, so the owner of one may be a standby for another.
const (
	RoleDaemon      = "daemon"       // the daemon's loops: poller, receiving, Cloudflare, backups
	RoleAPISweepers = "api-sweepers" // the api's durable sweepers and coordination cleanup
	RoleMailRuntime = "mail-runtime" // native outbox recovery and DKIM rotation
)

// Job is one periodic task. Every job runs on its own goroutine, so a slow one never delays
// another; Every 0 runs Run once for the whole ownership (a loop that owns its own timing).
type Job struct {
	Name  string
	Every time.Duration
	Run   func(context.Context)
}

// Locker elects the owner: state.Store's WithLock.
type Locker interface {
	WithLock(ctx context.Context, name string, work func(context.Context) error) (bool, error)
}

// Options are the election's timings.
type Options struct {
	// Retry is how often a process that does not own the role tries again.
	Retry time.Duration
	// Grace is how long a new owner waits before its first job: longer than the lock's probe
	// bound, so a predecessor whose session the database dropped has already seen its lock
	// lost and cancelled. Zero on SQLite, whose flock cannot be lost.
	Grace time.Duration
	// Elected, when set, is called as ownership begins and ends.
	Elected func(owner bool)
}

// Run keeps electing an owner of role until ctx ends; while this process owns it, it runs
// jobs. Losing the lock cancels every job's context, and Run waits for each to return before
// trying again, so this process never runs a job beside a later owner's. That is single
// ownership, not exactly-once: jobs keep their own fences (claims, leases, CAS moves).
func Run(ctx context.Context, locks Locker, role string, o Options, jobs []Job) {
	for ctx.Err() == nil {
		won, err := locks.WithLock(ctx, "role:"+role, func(owned context.Context) error {
			slog.Info("elected", "role", role)
			if o.Grace > 0 {
				select {
				case <-owned.Done():
					return context.Cause(owned)
				case <-time.After(o.Grace):
				}
			}
			if o.Elected != nil {
				o.Elected(true)
				defer o.Elected(false)
			}
			var wg sync.WaitGroup
			for _, j := range jobs {
				wg.Go(func() { loop(owned, j) })
			}
			wg.Wait()
			return context.Cause(owned)
		})
		if ctx.Err() != nil {
			return
		}
		if won {
			slog.Warn("stopped owning the role", "role", role, "error", errString(err))
		} else if err != nil {
			slog.Error("role election unavailable", "role", role, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(o.Retry):
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func loop(ctx context.Context, j Job) {
	if j.Every <= 0 {
		j.Run(ctx)
		return
	}
	ticker := time.NewTicker(j.Every)
	defer ticker.Stop()
	j.Run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		j.Run(ctx)
	}
}
