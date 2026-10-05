package app

import (
	"context"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Each API/daemon process may discover the same work: durable claims arbitrate.
// Cancellation stops new claims and shutdown joins bounded in-flight SMTP/finalization.
func startNativeOutbound(ctx context.Context, d runDeps) <-chan struct{} {
	done := make(chan struct{})
	if !d.nativeMail {
		close(done)
		return done
	}
	sender := sso.NativeOutbound{ConfigDir: d.configDir, StateRoot: d.stateDir, SecretDir: config.SecretDir(), Accounts: d.users, Domains: sso.NewNativeDomainStore(d.configDir), Settings: sso.NewStore(d.configDir), Logger: d.logger}
	return startNativeOutboundRuntime(ctx, d, sender)
}

func startNativeOutboundRuntime(ctx context.Context, d runDeps, sender sso.NativeOutbound) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			all, err := d.users.List()
			if err != nil {
				d.logger.Error("native outbox discovery deferred; retain queued mail", "error", "account storage unavailable")
			} else {
				// ponytail: four concurrent mailboxes, sequential jobs per owner; raise only
				// after measuring relay limits and storage contention with representative load.
				slots := make(chan struct{}, 4)
				var workers sync.WaitGroup
				for _, u := range all {
					if u.NativeMailboxSource == "" || u.NativeMailboxIssuer == "" {
						continue
					}
					select {
					case slots <- struct{}{}:
					case <-ctx.Done():
						workers.Wait()
						return
					}
					workers.Add(1)
					go func(userID string) {
						defer workers.Done()
						defer func() { <-slots }()
						ids, err := sender.Pending(ctx, userID, 50)
						if err != nil {
							if ctx.Err() == nil {
								d.logger.Error("native outbox discovery deferred; retain queued mail", "error", "mailbox storage unavailable")
							}
							return
						}
						for _, id := range ids {
							if ctx.Err() != nil {
								return
							}
							if err := sender.Recover(ctx, userID, id); err != nil && ctx.Err() == nil {
								d.logger.Error("native outbox recovery deferred; inspect delivery evidence before resubmitting", "correlation_id", id, "error", "storage, sender authority or relay unavailable")
							}
						}
					}(u.ID)
				}
				workers.Wait()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
