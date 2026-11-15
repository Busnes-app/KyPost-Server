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

// An import writes no authority or users, so it shares the directory fence
// with admissions and waits only for a directory writer.
func TestReceivingImportSharesTheDirectoryFence(t *testing.T) {
	r, created := receivingFixture(t)
	receive(t, r, "fenced", "one@example.test")
	imp := func(d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return r.importDelivery(ctx, "fenced")
	}
	writer, err := r.life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := imp(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("import did not wait for a directory writer:", err)
	}
	writer()
	reader, err := fsutil.LockFileShared(context.Background(), filepath.Join(r.configDir, "sso-lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader()
	if err := imp(5 * time.Second); err != nil {
		t.Fatal("import waited for an admission:", err)
	}
	if got := receivingInbox(t, r, created[0]); len(got) != 1 || got[0] != "fenced" {
		t.Fatal("import not delivered", got)
	}
}
