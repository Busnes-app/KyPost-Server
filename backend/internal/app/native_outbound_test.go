package app

import (
	"context"
	"testing"
	"time"
)

func TestNativeOutboundWorkerStopsWithoutWork(t *testing.T) {
	for _, native := range []bool{false, true} {
		d := newGracefulShutdownTestDeps(t)
		d.nativeMail = native
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := startNativeOutbound(ctx, d)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("outbox worker did not stop before claiming work")
		}
	}
}
