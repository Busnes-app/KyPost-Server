//go:build linux

package sso

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Admission only reads, so it shares the directory fence with other
// admissions and waits only for writers.
func TestNativeAdmissionSharesTheDirectoryFence(t *testing.T) {
	f := newAddressFixture(t, "one")
	id := f.ids["one"]
	admit := func(d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, id, id, f.accounts)
		return err
	}
	reader, err := f.life.lockDirectoryShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(time.Second); err != nil {
		t.Fatal("admission waited for another reader:", err)
	}
	reader()
	writer, err := f.life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := admit(50 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("admission did not wait for a writer:", err)
	}
	writer()
	mailbox.HoldAdmissionHandles(4)
	defer mailbox.HoldAdmissionHandles(0)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := admit(5 * time.Second); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

// A receiving bind or accept writes only the holding store, so it shares the
// directory fence like admission; mail access for an import stays exclusive.
func TestNativeHoldingAccessSharesTheDirectoryFence(t *testing.T) {
	f := newAddressFixture(t, "one")
	id := f.ids["one"]
	access := func(with func(context.Context, string, string, *users.Store, []string, func(map[string]NativeAssignment) error) error, d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return with(ctx, f.root, nativeIssuer, f.accounts, []string{id}, func(map[string]NativeAssignment) error { return nil })
	}
	reader, err := f.life.lockDirectoryShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := access(f.life.WithNativeHoldingAccess, time.Second); err != nil {
		t.Fatal("holding access waited for a reader:", err)
	}
	if err := access(f.life.WithNativeMailAccess, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("mail access did not wait for a reader:", err)
	}
	reader()
	writer, err := f.life.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer writer()
	if err := access(f.life.WithNativeHoldingAccess, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("holding access did not wait for a writer:", err)
	}
}
