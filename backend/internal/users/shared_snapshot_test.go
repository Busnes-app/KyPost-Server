//go:build linux

package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// A snapshot only reads, so it shares users.json's lock with snapshots in
// other processes and waits only for a writer; a writer waits for snapshots.
func TestCurrentUsersShareTheLockAndExcludeWriters(t *testing.T) {
	s, path := newCacheTestStore(t)
	snapshot := func(d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return s.WithCurrentUsers(ctx, func([]User) error { return nil })
	}
	reader, err := fsutil.LockFileShared(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot(time.Second); err != nil {
		t.Fatal("snapshot waited for another reader:", err)
	}
	wrote := make(chan struct{})
	go func() {
		_ = withWriteLock(path, func() error { close(wrote); return nil })
	}()
	select {
	case <-wrote:
		t.Fatal("writer ran beside a snapshot")
	case <-time.After(50 * time.Millisecond):
	}
	reader()
	<-wrote
	writer, err := fsutil.LockFileGated(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer()
	if err := snapshot(50 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("snapshot did not wait for a writer:", err)
	}
}
