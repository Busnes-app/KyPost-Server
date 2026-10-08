package mailbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// New growth (drafts and appends, imports, outgoing jobs) is refused inside
// the drive reserve with usage unchanged, though the quota has room. Mail
// already accepted by the receiving buffer still imports, replays still
// answer and a Sent copy reserved at queue time still files.
func TestMailboxWritesKeepTheDriveReserve(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	defer s.Close()
	delivered := Receipt{Gateway: "maddy-local", Delivery: "kept", Sender: "a@outside.test", Recipients: []Recipient{{Address: "one@example.test", Generation: 1}}}
	raw := "From: a@outside.test\r\nSubject: kept\r\n\r\nbody\r\n"
	if _, err := s.Import(ctx, delivered, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, nil))

	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 10<<30 + 1<<20, 100 << 30, nil } // 1 MiB beyond the reserve
	before, err := ReadUsage(dir)
	must(t, err)
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, fsutil.ErrDriveReserve) || !errors.Is(err, ErrCapacity) {
			t.Fatalf("%s inside the reserve: %v", what, err)
		}
		if after, _ := ReadUsage(dir); after != before {
			t.Fatalf("%s changed usage: %+v -> %+v", what, before, after)
		}
	}
	draft := "From: one@example.test\r\nTo: b@outside.test\r\nSubject: draft\r\n\r\nsmall\r\n"
	_, err = s.Append(ctx, "Drafts", strings.NewReader(draft), true)
	refused("draft", err)
	_, err = s.append(ctx, "INBOX", strings.NewReader(draft), "", "", "", false, &ImportMeta{})
	refused("import", err)
	other := outboxTestJob()
	refused("outgoing job", s.QueueOutbound(ctx, outboxMaster, "1f1de1d1-7362-461f-99c5-af09dca468d5", other))

	if id, err := s.Import(ctx, Receipt{Gateway: "maddy-local", Delivery: "new", Sender: "a@outside.test", Recipients: delivered.Recipients}, strings.NewReader(raw)); err != nil || id == 0 {
		t.Fatalf("accepted mail did not import: %v", err)
	}
	if _, err := s.Import(ctx, delivered, strings.NewReader(raw)); err != nil {
		t.Fatalf("delivery replay: %v", err)
	}
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob())) // exact replay
	if _, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err != nil {
		t.Fatalf("Sent copy refused after sending: %v", err)
	}
}
