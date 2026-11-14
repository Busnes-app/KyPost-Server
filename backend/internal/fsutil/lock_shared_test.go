package fsutil

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func tryLock(t *testing.T, lock func(context.Context, string) (func(), error), path string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	release, err := lock(ctx, path)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	return release
}

func TestLockFileSharedAdmitsReadersAndExcludesWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sso-lifecycle.json")
	first := tryLock(t, LockFileShared, path)
	second := tryLock(t, LockFileShared, path)
	if first == nil || second == nil {
		t.Fatal("two readers did not share the lock")
	}
	if tryLock(t, LockFileGated, path) != nil || tryLock(t, LockFileContext, path) != nil {
		t.Fatal("a writer acquired beside readers")
	}
	first()
	second()
	writer := tryLock(t, LockFileGated, path)
	if writer == nil {
		t.Fatal("writer refused after readers released")
	}
	if tryLock(t, LockFileShared, path) != nil {
		t.Fatal("a reader acquired beside a writer")
	}
	writer()
	legacy := tryLock(t, LockFileContext, path)
	if legacy == nil || tryLock(t, LockFileShared, path) != nil {
		t.Fatal("LockFileContext and LockFileShared do not exclude each other")
	}
	legacy()
	// A cancelled reader left neither the gate nor the lock held.
	if release := tryLock(t, LockFileGated, path); release == nil {
		t.Fatal("cancelled attempts retained a lock")
	} else {
		release()
	}
}

// Readers that always overlap never leave the lock free, so a writer gets in
// only because the gate stops new readers while it waits.
func TestLockFileGatedIsNotStarvedByOverlappingReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sso-lifecycle.json")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			time.Sleep(time.Duration(i) * time.Millisecond)
			for {
				select {
				case <-stop:
					return
				default:
				}
				release, err := LockFileShared(context.Background(), path)
				if err != nil {
					t.Error(err)
					return
				}
				time.Sleep(4 * time.Millisecond)
				release()
			}
		})
	}
	defer wg.Wait()
	defer close(stop)
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := LockFileGated(ctx, path)
	if err != nil {
		t.Fatal("writer starved:", err)
	}
	release()
}
