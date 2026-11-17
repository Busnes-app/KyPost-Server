package fsutil

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// At most cap(syncSlots) fsyncs run at once: each holds an OS thread, and the
// container's task limit is finite.
func TestSyncIsBounded(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entered atomic.Int32
	release := make(chan struct{})
	sync := syncFile
	syncFile = func(f *os.File) error {
		entered.Add(1)
		<-release
		return sync(f)
	}
	t.Cleanup(func() { syncFile = sync })
	slots := int32(cap(syncSlots))
	done := make(chan error, slots+8)
	for range slots + 8 {
		go func() { done <- Sync(f) }()
	}
	for deadline := time.Now().Add(5 * time.Second); entered.Load() < slots && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // room for an unbounded extra fsync to arrive
	n := entered.Load()
	close(release)
	for range slots + 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n != slots {
		t.Fatalf("%d fsyncs ran at once, want %d", n, slots)
	}
}
