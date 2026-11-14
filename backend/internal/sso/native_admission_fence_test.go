//go:build linux

package sso

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
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
