package mailbox

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkHasPendingOutbound is the per-mailbox, per-pass outbox hint on an
// empty queue: one read-only open, one query and a close.
func BenchmarkHasPendingOutbound(b *testing.B) {
	ctx := context.Background()
	dir := filepath.Join(b.TempDir(), "mailbox")
	s, err := Open(dir, testOwner, testLimits)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if pending, err := HasPendingOutbound(ctx, dir); err != nil || pending {
			b.Fatal(pending, err)
		}
	}
}
