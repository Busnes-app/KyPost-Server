package fsutil

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLockFileContextCancelsWithoutLaterAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	release, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if unlock, err := LockFileContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) || unlock != nil {
		t.Fatalf("cancel: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("unbounded contention")
	}
	release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	unlock, err := LockFileContext(ctx2, path)
	if err != nil {
		t.Fatal("cancelled waiter retained lock", err)
	}
	unlock()
	if unlock, err := LockFileContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) || unlock != nil {
		t.Fatal("expired context acquired", err)
	}
}
