package app

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// AC2: the SIGTERM sequence against a real listener. /readyz turns 503 at once while /livez
// keeps answering; on PostgreSQL the listener keeps serving
// for readinessDrainDelay, on SQLite it closes without that delay.
func TestGracefulStopDrainsBeforeClosing(t *testing.T) {
	d := newGracefulShutdownTestDeps(t)
	srv := newAPIServer(d, nil)
	srv.Prepare()
	go func() { _ = srv.Serve() }()
	base := "http://127.0.0.1:" + os.Getenv("WEB_PORT")
	client := &http.Client{Timeout: 2 * time.Second}
	status := func(method, path string) int {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	deadline := time.Now().Add(10 * time.Second)
	for status(http.MethodGet, "/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	readinessDrainDelay = 2 * time.Second
	t.Cleanup(func() { readinessDrainDelay = 5 * time.Second })

	stopped := make(chan struct{})
	var cancelled time.Time
	start := time.Now()
	go func() {
		gracefulStop(srv, func() { cancelled = time.Now() }, d.logger)
		close(stopped)
	}()
	// SQLite closes at once, so a probe may find the listener already gone.
	for c := status(http.MethodGet, "/readyz"); c != http.StatusServiceUnavailable && (c != 0 || dbbackend.Postgres()); c = status(http.MethodGet, "/readyz") {
		if time.Since(start) > time.Second {
			t.Fatalf("/readyz did not turn 503 at SIGTERM: %d", c)
		}
	}
	if !dbbackend.Postgres() {
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("SQLite shutdown waited")
		}
		if cancelled.Sub(start) > time.Second {
			t.Fatalf("SQLite added a drain delay: work cancelled after %v", cancelled.Sub(start))
		}
		return
	}
	for time.Since(start) < readinessDrainDelay-500*time.Millisecond {
		if c := status(http.MethodGet, "/readyz"); c != http.StatusServiceUnavailable {
			t.Fatalf("/readyz during the drain: %d", c)
		}
		if c := status(http.MethodGet, "/livez"); c != http.StatusOK {
			t.Fatalf("/livez during the drain: %d", c)
		}
		// An ordinary authenticated route still reaches its handler (401 without a cookie).
		if c := status(http.MethodGet, "/api/config"); c != http.StatusUnauthorized {
			t.Fatalf("an ordinary route during the drain: %d", c)
		}
		time.Sleep(100 * time.Millisecond)
	}
	<-stopped
	if took := cancelled.Sub(start); took < readinessDrainDelay {
		t.Fatalf("PostgreSQL work cancelled after %v, before the %v drain", took, readinessDrainDelay)
	}
	if c := status(http.MethodGet, "/livez"); c != 0 {
		t.Fatalf("listener still open after the stop: %d", c)
	}
}
