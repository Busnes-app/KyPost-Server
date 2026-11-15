//go:build linux

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// A delivery only reads the domain set, so it shares the domain fence with
// other deliveries and waits only for a writer.
func TestReceivingSharesTheDomainFence(t *testing.T) {
	r, _ := receivingFixture(t)
	fence := filepath.Join(r.configDir, sso.NativeDomainsFile)
	bind := func(id string, d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return r.bind(ctx, id, "sender@outside.test", "one@example.test")
	}
	reader, err := fsutil.LockFileShared(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := bind("beside-a-reader", 5*time.Second); err != nil {
		t.Fatal("delivery waited for another reader:", err)
	}
	reader()
	writer, err := fsutil.LockFileGated(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := bind("behind-a-writer", 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("delivery did not wait for a writer:", err)
	}
	writer()
}
