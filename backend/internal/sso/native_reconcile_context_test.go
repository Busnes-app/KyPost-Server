//go:build linux

package sso

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func TestNativeReconcileContextCancelsPreparationLock(t *testing.T) {
	root := t.TempDir()
	life := NewLifecycleStore(t.TempDir())
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	target := filepath.Join(root, "users", "reserved-local")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := fsutil.LockFile(target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = life.ReconcileNativeMailboxContext(ctx, root, nativeIssuer, "one", "reserved-local", "example.test", nativeLimits)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preparation contention ignored context: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("cancelled preparation published storage: %v", err)
	}
}
