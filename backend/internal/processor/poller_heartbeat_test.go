package processor

import (
	"os"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/health"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestDaemonHeartbeatContinuesDuringBlockedTick(t *testing.T) {
	mail := &blockingMailbox{entered: make(chan struct{}, 1)}
	p, user := newTickTestPoller(t, mail)
	p.cfg.Scan.IntervalSeconds = 1
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	// The injected client still uses the ordinary configured-mailbox selection.
	if err := os.WriteFile(p.userIMAPConfigPath(user.ID), []byte("test client"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.globalStore = store
	p.Start()
	t.Cleanup(func() {
		p.Stop()
		done := make(chan struct{})
		go func() { p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("shutdown did not join the blocked tick and health publisher")
		}
		_ = store.Close()
	})
	select {
	case <-mail.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled tick did not enter the blocking mailbox")
	}
	initial, ok := health.DecodeDaemonReport(store.DaemonHealth())
	if !ok {
		t.Fatal("missing eager daemon report")
	}
	// A live heartbeat must carry an observed failure, not invent a healthy state.
	p.health.MarkUnhealthy("test mailbox failure")
	p.health.RecordClassifierFailure()
	deadline := time.Now().Add(daemonHeartbeatInterval + 5*time.Second)
	for time.Now().Before(deadline) {
		report, valid := health.DecodeDaemonReport(store.DaemonHealth())
		if valid && report.AtUTC != initial.AtUTC {
			if report.PollingHealthy || !report.ClassifierFailing {
				t.Fatal("heartbeat concealed a subsystem failure")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("long poll tick prevented the independent daemon heartbeat")
}
