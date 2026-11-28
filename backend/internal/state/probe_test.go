package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

// gatedConnector holds every Ping until the test opens gate, then runs the real one: a ping
// in flight when the owner's work ends or the probe times out, as the mail role flapped in
// the KyCalendar #244 e2e run.
type gatedConnector struct {
	driver.Connector
	entered chan struct{}
	gate    chan struct{}
	open    func() // opens gate, once
}

func (c *gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	return &gatedConn{conn, c}, err
}

type gatedConn struct {
	driver.Conn
	c *gatedConnector
}

func (g *gatedConn) Ping(ctx context.Context) error {
	g.c.entered <- struct{}{}
	<-g.c.gate
	return g.Conn.(driver.Pinger).Ping(ctx)
}

func gatedLockConn(t *testing.T) (*sql.Conn, *gatedConnector) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	base, err := pgdialect.NewConnector(os.Getenv(dbbackend.EnvTestURL), false, "")
	if err != nil {
		t.Fatal(err)
	}
	gc := &gatedConnector{Connector: base, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	gc.open = sync.OnceFunc(func() { close(gc.gate) })
	db := sql.OpenDB(gc)
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(gc.open) // runs first: a failed test never leaves a ping held
	return conn, gc
}

func shortProbe(t *testing.T, timeout time.Duration) {
	oldInterval, oldTimeout := lockProbeInterval, lockProbeTimeout
	lockProbeInterval, lockProbeTimeout = 10*time.Millisecond, timeout
	t.Cleanup(func() { lockProbeInterval, lockProbeTimeout = oldInterval, oldTimeout })
}

// The release unlocks and discards the lock's conn once the probe returns. A ping still in
// flight then closes that conn under the unlock, which panicked with a nil pointer in
// database/sql (execDC from unlockAndDiscard). The probe must return, and free a fence
// session's mutex, only after its ping has.
func TestProbeWaitsForItsPingBeforeRelease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		timeout    time.Duration
		ownerQuits bool
	}{
		{"owner's work ends mid-ping", time.Minute, true},
		{"ping times out", 20 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortProbe(t, tc.timeout)
			conn, gc := gatedLockConn(t)
			owned, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			var busy sync.Mutex
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				probe(owned, conn, cancel, &busy)
			}()
			<-gc.entered
			if tc.ownerQuits {
				cancel(nil)
			} else {
				// Ownership still ends at once, while the ping hangs.
				select {
				case <-owned.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("a ping past its timeout did not end ownership")
				}
				if !errors.Is(context.Cause(owned), ErrOwnershipLost) {
					t.Fatalf("cause = %v", context.Cause(owned))
				}
			}
			select {
			case <-returned:
				t.Fatal("probe returned while its ping still used the lock's conn")
			case <-time.After(200 * time.Millisecond):
			}
			if busy.TryLock() {
				t.Fatal("probe freed the fence session while its ping still used the conn")
			}
			gc.open()
			<-returned
			// The release that follows must not panic, whatever the ping did to the conn.
			s := &Store{}
			s.unlockAndDiscard(conn, spaceLock, "role:x")
		})
	}
}
