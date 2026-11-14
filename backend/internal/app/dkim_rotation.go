package app

import (
	"context"
	"net"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// Package tests shorten the interval and replace DNS and the clock.
var (
	dkimRotationInterval                     = time.Hour
	dkimResolver         mailmsg.DNSResolver = net.DefaultResolver
	dkimNow                                  = time.Now
)

// startDKIMRotation runs the DKIM rotation tick once at start, which is how
// an interrupted transition resumes, then every dkimRotationInterval. Each
// process may run it: the native-domain lock and a re-read of the sealed
// profile make concurrent ticks safe. A relay profile is left alone.
func startDKIMRotation(ctx context.Context, d runDeps) <-chan struct{} {
	done := make(chan struct{})
	if !d.nativeMail {
		close(done)
		return done
	}
	rotation := api.DKIMRotationFor(d.configDir, dkimResolver)
	go func() {
		defer close(done)
		ticker := time.NewTicker(dkimRotationInterval)
		defer ticker.Stop()
		for {
			_, events, err := rotation.Tick(ctx, dkimNow())
			api.LogDKIMEvents(d.logger, events)
			if err != nil && ctx.Err() == nil {
				d.logger.Error("dkim rotation deferred; the current key keeps signing", "error", "relay profile or its lock unavailable")
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
