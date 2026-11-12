package mailbox

import (
	"context"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

func TestNativeOutboxReadOnlyDiscovery(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	check := func(want bool) {
		t.Helper()
		hint, err := HasPendingOutbound(ctx, dir)
		must(t, err)
		ids, err := s.PendingOutbound(ctx, 50)
		must(t, err)
		if hint != want || (len(ids) > 0) != want {
			t.Fatalf("hint=%v jobs=%v want work=%v", hint, ids, want)
		}
	}
	check(false)
	// A different connection's enqueue must be visible without any cache refresh.
	writer := openTest(t, dir, testOwner, testLimits)
	must(t, writer.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	check(true)
	_, claim, err := writer.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	check(false) // Crashed/submitting work must never be automatically replayed.
	must(t, writer.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, &textproto.Error{Code: 451, Msg: "fixture"}))
	check(false) // Future backoff remains future.
	if m := readIdle(t, dir); m.Until != retryAt(t, writer) {
		t.Fatalf("idle mark until %d, want the retry time", m.Until)
	}
	// The retry comes due: move it and the mark it bounded into the past.
	_, err = writer.db.Exec("UPDATE outbox_deliveries SET next_attempt=?", time.Now().Add(-time.Minute).Unix())
	must(t, err)
	must(t, os.Remove(filepath.Join(dir, OutboundIdleFile)))
	check(true)
	_, claim, err = writer.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, writer.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
	check(true) // Accepted Sent-only obligations must still be discovered.
	_, err = writer.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	check(false)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = HasPendingOutbound(canceled, dir); err == nil {
		t.Fatal("canceled query succeeded")
	}
}

func TestNativeOutboxReadOnlyDiscoveryIndependentAndRelay(t *testing.T) {
	for _, independent := range []bool{false, true} {
		ctx := context.Background()
		dir := filepath.Join(t.TempDir(), "mailbox")
		s := openTest(t, dir, testOwner, testLimits)
		job := independentJob()
		job.Independent = independent
		must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
		_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
		must(t, err)
		must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, mailmsg.ErrSMTPAcceptanceUncertain))
		hint, err := HasPendingOutbound(ctx, dir)
		must(t, err)
		if hint != independent {
			t.Fatal("uncertain primary discovery", independent, hint)
		}
	}
}

func TestNativeOutboxReadOnlyDiscoveryStorageFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := HasPendingOutbound(context.Background(), dir); err == nil {
		t.Fatal("missing database treated as empty")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("read-only probe created storage", err)
	}
	must(t, os.Mkdir(dir, 0700))
	must(t, os.WriteFile(filepath.Join(dir, "mailbox.db"), []byte("not a SQLite database"), 0600))
	if _, err := HasPendingOutbound(context.Background(), dir); err == nil {
		t.Fatal("corruption treated as empty")
	}
}
