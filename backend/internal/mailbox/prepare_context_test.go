//go:build linux

package mailbox

import (
	"context"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareAccountContextContendedCancellation(t *testing.T) {
	root := t.TempDir()
	owner := preparationOwner()
	target := filepath.Join(root, "users", owner.Mailbox)
	release, err := fsutil.LockFile(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err = PrepareAccountContext(ctx, root, owner, "one@example.test", preparationLimits()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	release()
	if _, err = os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled published", err)
	}
	if _, err = PrepareAccount(root, owner, "one@example.test", preparationLimits()); err != nil {
		t.Fatal("cancel abandoned lock", err)
	}
}
