package mailbox

import (
	"context"
	"errors"
	"net/textproto"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

func independentJob() OutboundJob {
	job := outboxTestJob()
	job.Independent = true
	original := job.Deliveries[0]
	job.Deliveries = nil
	for _, recipient := range original.Recipients {
		job.Deliveries = append(job.Deliveries, OutboundDelivery{Recipients: []string{recipient}, Raw: original.Raw})
	}
	return job
}

func TestNativeOutboxIndependentRecipientRestartAndSent(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	job := independentJob()
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, mailmsg.ErrSMTPAcceptanceUncertain))
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	pending, err := s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 1 || pending[0] != outboxTestID {
		t.Fatal("uncertain primary blocked unrelated recipient", pending)
	}
	delivery, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 1, outboxGeneration)
	must(t, err)
	if len(delivery.Recipients) != 1 || delivery.Recipients[0] != "hidden@outside.test" {
		t.Fatal("wrong recipient claim")
	}
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 1, claim, nil))
	sent, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	again, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if sent != again {
		t.Fatal("duplicate Sent filing")
	}
	pending, err = s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("accepted or uncertain recipient requeued", pending)
	}
	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("uncertain receipt retried")
	}
	current, statuses, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if !current.Independent || statuses[0].State != "uncertain" || statuses[1].State != "accepted" {
		t.Fatal("independent outcomes lost", statuses)
	}
	relay := mailmsg.DomainRelay{Mode: "direct", Generation: outboxGeneration, Domains: []string{"example.test"}, Issuer: testOwner.Issuer, Host: "mail.example.test"}
	present, err := ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), outboxMaster, relay)
	must(t, err)
	if !present {
		t.Fatal("snapshot missed independent queue")
	}
	_, err = s.db.Exec("DELETE FROM outbox_independent")
	must(t, err)
	if _, _, _, err = s.ReadOutbound(ctx, outboxMaster, outboxTestID); err == nil {
		t.Fatal("missing discovery marker accepted")
	}
}

func TestNativeOutboxIndependentTerminalCompaction(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	job := independentJob()
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	for sequence := range job.Deliveries {
		_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, sequence, outboxGeneration)
		must(t, err)
		var result error
		if sequence == 0 {
			result = &textproto.Error{Code: 550, Msg: "controlled refusal"}
		}
		must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, sequence, claim, result))
	}
	_, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	receipt, statuses, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if receipt.intent == nil || !receipt.Independent || statuses[0].State != "failed" || statuses[1].State != "accepted" {
		t.Fatal("compaction lost direct outcomes", statuses)
	}
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	malformed := outboxTestJob()
	malformed.Independent = true
	if err = s.QueueOutbound(ctx, outboxMaster, "0f1de1d1-7362-461f-99c5-af09dca468d6", malformed); !errors.Is(err, ErrOutbound) {
		t.Fatal("grouped direct delivery accepted", err)
	}
}
