//go:build linux

package mailbox

import (
	"path/filepath"
	"sync"
	"testing"
)

// Admission runs under a shared directory fence, so one open client's held
// storage is checked by concurrent requests: the first opens its handles.
func TestHeldCheckIsSafeConcurrently(t *testing.T) {
	root := t.TempDir()
	owner, limits := preparationOwner(), preparationLimits()
	if _, err := PrepareAccount(root, owner, "one@example.test", limits); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "users")
	held := &Prepared{dir: filepath.Join(parent, owner.Mailbox)}
	defer held.Close()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := held.ValidatePreparedMailbox(parent, owner, "one@example.test", limits); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
