package mailbox

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The org pool check (tenancy design "Quotas") runs on every write that adds bytes: a
// client append and a send's enqueue. It never runs on a delivery receiving already
// accepted, on filing a Sent copy reserved at enqueue, or on an exact replay. A refusal
// writes nothing, and it is ErrCapacity. With no check installed (single-tenant) nothing
// is asked.
func TestPoolCheck(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	var asked []int64
	full := false
	SetPoolCheck(func(_ context.Context, owner Owner, add int64) error {
		if owner != testOwner {
			t.Errorf("checked for %+v", owner)
		}
		asked = append(asked, add)
		if full {
			return ErrPoolFull
		}
		return nil
	})
	t.Cleanup(func() { SetPoolCheck(nil) })

	if _, err := s.Append(ctx, "Drafts", bytes.NewReader(testRaw), true); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != int64(len(testRaw)) {
		t.Fatalf("append asked %v, want [%d]", asked, len(testRaw))
	}

	job := outboxTestJob()
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	if len(asked) != 2 || asked[1] <= int64(len(job.Sent)) {
		t.Fatalf("enqueue asked %v: want the sealed job plus its Sent copy", asked)
	}

	// Over the pool from here on.
	full = true
	before := outboxUsage(t, s)
	if _, err := s.Append(ctx, "Drafts", bytes.NewReader(testRaw), true); !errors.Is(err, ErrPoolFull) || !errors.Is(err, ErrCapacity) {
		t.Fatalf("append over the pool: %v", err)
	}
	other := outboxTestJob()
	if err := s.QueueOutbound(ctx, outboxMaster, "33333333-3333-4333-8333-333333333333", other); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("enqueue over the pool: %v", err)
	}
	if after := outboxUsage(t, s); after != before {
		t.Fatalf("a refused write changed usage: %d -> %d", before, after)
	}

	n := len(asked)
	// An exact replay of the queued job adds nothing.
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	// The send enqueued under the pool still files its Sent copy.
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
	if _, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err != nil {
		t.Fatalf("Sent copy reserved at enqueue refused over the pool: %v", err)
	}
	// Mail receiving accepted imports while over the pool.
	if _, err := s.Import(ctx, testReceipt("held"), bytes.NewReader(testRaw)); err != nil {
		t.Fatalf("held delivery refused over the pool: %v", err)
	}
	if len(asked) != n {
		t.Fatalf("replay, Sent filing or held import asked the pool: %v", asked[n:])
	}

	// Single-tenant: no check installed, nothing asked, the write succeeds.
	SetPoolCheck(nil)
	if _, err := s.Append(ctx, "Drafts", bytes.NewReader(testRaw), true); err != nil || len(asked) != n {
		t.Fatalf("append with no pool: %v, asked %v", err, asked[n:])
	}
}
