package mailbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

var outboxMaster = bytes.Repeat([]byte{7}, 32)

const outboxTestID = "0f1de1d1-7362-461f-99c5-af09dca468d5"
const outboxGeneration = "c2c1e518-ad50-455e-89e4-59ce90c4e7f3"

func outboxTestJob() OutboundJob {
	raw := []byte("From: alice@example.test\r\nTo: visible@outside.test\r\nSubject: private signed-only subject\r\nContent-Type: multipart/signed; boundary=signature\r\n\r\n--signature\r\nPRIVATE-SIGNED-ONLY-WIRE\r\n--signature--\r\n")
	sent := []byte("From: alice@example.test\r\nTo: visible@outside.test\r\nBcc: hidden@outside.test\r\nSubject: encrypted message\r\nContent-Type: multipart/encrypted; protocol=application/pgp-encrypted; boundary=enc\r\n\r\n--enc\r\nopaque encrypted Sent\r\n--enc--\r\n")
	return OutboundJob{From: "alice@example.test", RelayGeneration: outboxGeneration, Deliveries: []OutboundDelivery{{Recipients: []string{"visible@outside.test", "hidden@outside.test"}, Raw: raw}}, Sent: sent}
}

func TestNativeOutboxEncryptedRestartClaimsAndSent(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	job := outboxTestJob()
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	var sealed []byte
	must(t, s.db.QueryRow("SELECT ciphertext FROM outbox").Scan(&sealed))
	for _, secret := range []string{job.From, "hidden@outside.test", "PRIVATE-SIGNED-ONLY-WIRE", "opaque encrypted Sent"} {
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatal("queued plaintext escaped encryption")
		}
	}
	changed := outboxTestJob()
	changed.Deliveries[0].Raw = append(changed.Deliveries[0].Raw, []byte("changed")...)
	if !errors.Is(s.QueueOutbound(ctx, outboxMaster, outboxTestID, changed), ErrConflict) {
		t.Fatal("adopted changed intent")
	}
	if _, _, _, err := s.ReadOutbound(ctx, bytes.Repeat([]byte{8}, 32), outboxTestID); err == nil {
		t.Fatal("wrong key accepted")
	}
	other := openTest(t, filepath.Join(t.TempDir(), "other"), testOwner, testLimits)
	must(t, other.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	_, err := other.db.Exec("UPDATE outbox SET ciphertext=?", sealed)
	must(t, err)
	if _, _, _, err = other.ReadOutbound(ctx, outboxMaster, outboxTestID); err == nil {
		t.Fatal("copied foreign namespace accepted")
	}
	// Two independent SQLite writers compete for one durable authorization claim.
	second := openTest(t, dir, testOwner, testLimits)
	var wg sync.WaitGroup
	claims := make(chan string, 2)
	for _, writer := range []*Store{s, second} {
		wg.Add(1)
		go func(writer *Store) {
			defer wg.Done()
			delivery, claim, err := writer.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
			if err == nil {
				if !bytes.Equal(delivery.Raw, job.Deliveries[0].Raw) {
					t.Error("wire changed")
				}
				claims <- claim
			}
		}(writer)
	}
	wg.Wait()
	close(claims)
	var claim string
	count := 0
	for c := range claims {
		claim = c
		count++
	}
	if count != 1 {
		t.Fatal("claim count", count)
	}
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	_, states, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if states[0].State != "submitting" {
		t.Fatal("claim not durable")
	}
	pending, err := s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("crashed submitter scheduled for replay")
	}

	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("reclaimed interrupted submission")
	}
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, mailmsg.ErrSMTPAcceptedThenFailed))
	pending, err = s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 1 {
		t.Fatal("accepted Sent obligation not scheduled")
	}

	_, err = s.db.Exec("DELETE FROM folders WHERE name='Sent'")
	must(t, err)
	if _, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err == nil {
		t.Fatal("filed into missing Sent folder")
	}
	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("Sent failure authorized duplicate SMTP")
	}
	must(t, s.CreateFolder(ctx, "Sent"))
	sent, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	again, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if sent != again {
		t.Fatal("duplicate Sent copy")
	}
	pending, err = s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("completed delivery scheduled again")
	}

	must(t, s.Move(ctx, "Sent", sent, "Archive"))
	must(t, s.Delete(ctx, "Archive", sent))
	again, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if sent != again {
		t.Fatal("resurrected deleted Sent")
	}
	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("resent accepted mail")
	}
}

func TestNativeOutboxUncertainRetryAndCorruption(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		submission error
		want       string
	}{{"lostACK", mailmsg.ErrSMTPAcceptanceUncertain, "uncertain"}, {"unknown", errors.New("provider echo PRIVATE"), "uncertain"}, {"temporary", &textproto.Error{Code: 451, Msg: "provider PRIVATE"}, "retryable"}, {"permanent", &textproto.Error{Code: 550, Msg: "PRIVATE"}, "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
			must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
			_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
			must(t, err)
			must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, tc.submission))
			_, states, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
			must(t, err)
			if states[0].State != tc.want {
				t.Fatal(states)
			}
			if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
				t.Fatal("unsafe or premature retry")
			}
			if tc.want == "retryable" {
				for attempt := 2; attempt <= 6; attempt++ {
					_, err = s.db.Exec("UPDATE outbox_deliveries SET next_attempt=0")
					must(t, err)
					_, claim, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
					must(t, err)
					must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, tc.submission))
				}
				_, states, _, err = s.ReadOutbound(ctx, outboxMaster, outboxTestID)
				must(t, err)
				if states[0].State != "failed" || states[0].Attempts != 6 {
					t.Fatal("retry ceiling", states)
				}
			}
		})
	}
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	if _, _, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, "d3c1e518-ad50-455e-89e4-59ce90c4e7f3"); err == nil {
		t.Fatal("adopted rotated relay")
	}
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	_, err = s.db.Exec("UPDATE outbox_deliveries SET attempts=0")
	must(t, err)
	if err = s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, &textproto.Error{Code: 451, Msg: "retry"}); err == nil {
		t.Fatal("corrupt attempts accepted")
	}
	var sealed []byte
	must(t, s.db.QueryRow("SELECT ciphertext FROM outbox").Scan(&sealed))
	var envelope map[string]any
	must(t, json.Unmarshal(sealed, &envelope))
	envelope["nonce"] = "AA=="
	sealed, err = json.Marshal(envelope)
	must(t, err)
	_, err = s.db.Exec("UPDATE outbox SET ciphertext=?", sealed)
	must(t, err)
	if _, _, _, err = s.ReadOutbound(ctx, outboxMaster, outboxTestID); err == nil {
		t.Fatal("corrupt nonce accepted")
	}
}

func TestNativeOutboxQuotaBackfillAndSharedCapacity(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	limits := testLimits
	limits.Records = 3
	s := openTest(t, dir, testOwner, limits)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	var count, used int64
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage").Scan(&count, &used))
	if count != 3 || used == 0 {
		t.Fatal("no reserved capacity")
	}
	if _, err := s.Append(ctx, "INBOX", bytes.NewReader(testRaw), false); !errors.Is(err, ErrCapacity) {
		t.Fatal("mail ignored queued capacity", err)
	}
	_, err := s.db.Exec("DELETE FROM usage")
	must(t, err)
	must(t, s.Close())
	s = openTest(t, dir, testOwner, limits)
	var afterCount, afterUsed int64
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage").Scan(&afterCount, &afterUsed))
	if afterCount != count || afterUsed != used {
		t.Fatal("backfill lost outbox quota", afterCount, afterUsed, count, used)
	}
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
	_, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage").Scan(&afterCount, &afterUsed))
	// Filing Sent consumes its reservation and compacts the finished job.
	if afterCount != count || afterUsed >= used {
		t.Fatal("Sent did not consume its reservation", afterCount, afterUsed, count, used)
	}
	malformed := outboxTestJob()
	malformed.Deliveries[0].Raw = []byte("From: alice@example.test\r\nBcc: hidden@outside.test\r\n\r\nbody\r\n")
	if err = s.QueueOutbound(ctx, outboxMaster, "0f1de1d1-7362-461f-99c5-af09dca468d6", malformed); !errors.Is(err, ErrOutbound) {
		t.Fatal("Bcc leaked into wire")
	}
}

func TestNativeOutboxClaimCrashHelper(t *testing.T) {
	dir := os.Getenv("KYPOST_OUTBOX_CRASH_DIR")
	if dir == "" {
		return
	}
	s, err := Open(dir, testOwner, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ClaimOutbound(context.Background(), outboxMaster, outboxTestID, 0, outboxGeneration)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stdout.WriteString("CLAIM_COMMITTED\n")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Hour)
}

func TestNativeOutboxKilledSubmitterCannotReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	must(t, s.Close())
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeOutboxClaimCrashHelper$")
	child.Env = append(os.Environ(), "KYPOST_OUTBOX_CRASH_DIR="+dir)
	output, err := child.StdoutPipe()
	must(t, err)
	must(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	line, err := bufio.NewReader(output).ReadString('\n')
	must(t, err)
	if line != "CLAIM_COMMITTED\n" {
		t.Fatal("child did not commit claim", line)
	}
	must(t, child.Process.Kill())
	if child.Wait() == nil {
		t.Fatal("child was not killed")
	}
	s = openTest(t, dir, testOwner, testLimits)
	_, states, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if states[0].State != "submitting" {
		t.Fatal("claim lost on kill")
	}
	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("killed submitter reauthorized")
	}
}

func TestNativeOutboxSnapshotRefusesOrphanClaims(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	_, err := s.db.Exec("PRAGMA foreign_keys=OFF; DELETE FROM outbox")
	must(t, err)
	must(t, s.Close())
	relay := mailmsg.DomainRelay{Generation: outboxGeneration, Domains: []string{"example.test"}, Issuer: testOwner.Issuer, Host: "smtp.provider.test", Port: 465, Username: "operator", Password: "secret"}
	if _, err = ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), outboxMaster, relay); err == nil {
		t.Fatal("orphan claim granted clean snapshot")
	}
}

func TestNativeOutboxSnapshotRefusesPartialSchema(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	_, err := s.db.Exec("PRAGMA foreign_keys=OFF; DROP TABLE outbox")
	must(t, err)
	must(t, s.Close())
	if _, err = ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), nil, mailmsg.DomainRelay{}); err == nil {
		t.Fatal("partial outbox schema treated as legacy")
	}
}

func TestNativeOutboxDiscoveryRequiresAcceptedPrimary(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	job := outboxTestJob()
	job.Sent = nil
	job.Deliveries = append(job.Deliveries, OutboundDelivery{Recipients: []string{"other@outside.test"}, Raw: job.Deliveries[0].Raw})
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	pending, err := s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("follow-ons scheduled behind submitting primary", pending)
	}
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
	pending, err = s.PendingOutbound(ctx, 10)
	must(t, err)
	if len(pending) != 1 || pending[0] != outboxTestID {
		t.Fatal("accepted primary did not release follow-ons", pending)
	}
}

// A quota lowered below what a mailbox holds must not fail its backups.
func TestNativeOutboxSnapshotAcceptsUsageAboveLoweredQuota(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, outboxTestJob()))
	_, err := s.db.Exec("UPDATE identity SET payload_bytes=1")
	must(t, err)
	must(t, s.Close())
	relay := mailmsg.DomainRelay{Generation: outboxGeneration, Domains: []string{"example.test"}, Issuer: testOwner.Issuer, Host: "smtp.provider.test", Port: 465, Username: "operator", Password: "secret"}
	if has, err := ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), outboxMaster, relay); err != nil || !has {
		t.Fatalf("over-quota snapshot refused: %v %v", has, err)
	}
}

// outboxUsage returns usage.payload_bytes after checking it against the
// equation ValidateOutboundSnapshot and the open-time backfill use.
func outboxUsage(t *testing.T, s *Store) int64 {
	t.Helper()
	var used, want int64
	must(t, s.db.QueryRow(`SELECT (SELECT payload_bytes FROM usage WHERE id=1),
 (SELECT coalesce(sum(length(raw)),0) FROM messages)+(SELECT coalesce(sum(length(ciphertext)+sent_bytes),0) FROM outbox)`).Scan(&used, &want))
	if used != want {
		t.Fatalf("usage %d, recomputed %d", used, want)
	}
	return used
}

func outboxCiphertext(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	var size int64
	must(t, s.db.QueryRow("SELECT length(ciphertext) FROM outbox WHERE id=?", id).Scan(&size))
	return size
}

// A finished job (every delivery accepted, Sent filed) shrinks to a sealed
// receipt; status, Sent retry, replay and snapshot validation keep working.
func TestNativeOutboxCompactsFinishedJob(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	job := outboxTestJob()
	job.Deliveries[0].Raw = append(job.Deliveries[0].Raw[:len(job.Deliveries[0].Raw)-len("--signature--\r\n")], []byte(strings.Repeat("PRIVATE-SIGNED-ONLY-WIRE\r\n", 2000)+"--signature--\r\n")...)
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	whole := outboxCiphertext(t, s, outboxTestID)
	queued := outboxUsage(t, s)
	_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
	must(t, err)
	must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
	if outboxCiphertext(t, s, outboxTestID) != whole {
		t.Fatal("compacted before Sent was filed")
	}
	sent, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	receipt := outboxCiphertext(t, s, outboxTestID)
	if receipt >= 1024 || receipt >= whole {
		t.Fatal("finished job not compacted", whole, receipt)
	}
	if used := outboxUsage(t, s); used != int64(len(job.Sent))+receipt || used >= queued {
		t.Fatal("usage after compaction", queued, used, receipt)
	}
	var sealed []byte
	must(t, s.db.QueryRow("SELECT ciphertext FROM outbox").Scan(&sealed))
	for _, secret := range []string{job.From, "visible@outside.test", "hidden@outside.test", "PRIVATE-SIGNED-ONLY-WIRE"} {
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatal("receipt plaintext escaped encryption")
		}
	}
	got, statuses, gotSent, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
	must(t, err)
	if got.From != job.From || got.RelayGeneration != outboxGeneration || len(got.Deliveries) != 1 || got.Deliveries[0].Raw != nil || got.Deliveries[0].Recipients != nil || got.Sent != nil || gotSent != sent || len(statuses) != 1 || statuses[0].State != "accepted" {
		t.Fatalf("compacted read %+v %+v %d", got, statuses, gotSent)
	}
	if again, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err != nil || again != sent {
		t.Fatal("Sent retry after compaction", again, err)
	}
	must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, job))
	changed := outboxTestJob()
	if !errors.Is(s.QueueOutbound(ctx, outboxMaster, outboxTestID, changed), ErrConflict) {
		t.Fatal("compacted job adopted a different intent")
	}
	if _, _, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration); err == nil {
		t.Fatal("compacted job claimable")
	}
	if s.QuarantineOutbound(ctx, outboxTestID, 0) == nil {
		t.Fatal("compacted delivery quarantined")
	}
	if pending, err := s.PendingOutbound(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatal("compacted job scheduled", pending, err)
	}
	if from, err := s.QueuedOutboundFrom(ctx, outboxMaster); err != nil || len(from) != 0 {
		t.Fatal("compacted job counted as queued", from, err)
	}
	if outboxCiphertext(t, s, outboxTestID) != receipt {
		t.Fatal("receipt rewritten")
	}
	// The Sent copy may move or go; the receipt still answers.
	must(t, s.Move(ctx, "Sent", sent, "Archive"))
	must(t, s.Delete(ctx, "Archive", sent))
	if again, err := s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err != nil || again != sent {
		t.Fatal("Sent retry after deletion", again, err)
	}
	if _, err := s.db.Exec("DELETE FROM usage"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	outboxUsage(t, s)
	must(t, s.Close())
	relay := mailmsg.DomainRelay{Generation: outboxGeneration, Domains: []string{"example.test"}, Issuer: testOwner.Issuer, Host: "smtp.provider.test", Port: 465, Username: "operator", Password: "secret"}
	if has, err := ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), outboxMaster, relay); err != nil || !has {
		t.Fatalf("snapshot with a compacted job refused: %v %v", has, err)
	}
	s = openTest(t, dir, testOwner, testLimits)
	if _, err := s.db.Exec("UPDATE outbox_deliveries SET state='uncertain'"); err != nil {
		t.Fatal(err)
	}
	must(t, s.Close())
	if _, err := ValidateOutboundSnapshot(ctx, filepath.Join(dir, "mailbox.db"), outboxMaster, relay); err == nil {
		t.Fatal("receipt with an unfinished delivery accepted")
	}
}

// Only definite outcomes with Sent filed (or no Sent copy) finish a job.
func TestNativeOutboxKeepsUnfinishedJobsWhole(t *testing.T) {
	ctx := context.Background()
	noSent := outboxTestJob()
	noSent.Sent = nil
	for _, tc := range []struct {
		name       string
		submission error
		compacted  bool
	}{{"uncertain", mailmsg.ErrSMTPAcceptanceUncertain, false}, {"unknown", errors.New("lost"), false}, {"retryable", &textproto.Error{Code: 451, Msg: "later"}, false}, {"failed", &textproto.Error{Code: 550, Msg: "no"}, true}, {"accepted", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
			must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, noSent))
			whole := outboxCiphertext(t, s, outboxTestID)
			_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
			must(t, err)
			must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, tc.submission))
			outboxUsage(t, s)
			job, _, _, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID)
			must(t, err)
			if (outboxCiphertext(t, s, outboxTestID) < whole) != tc.compacted || (job.Deliveries[0].Raw == nil) != tc.compacted {
				t.Fatal("compaction", tc.compacted, job.Deliveries[0].Raw == nil)
			}
			if _, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID); err == nil {
				t.Fatal("filed a Sent copy the job never had")
			}
			must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, noSent))
		})
	}
	// Two deliveries with Sent: filing after the primary, an uncertain or
	// quarantined follow-on keeps the job whole; an accepted one finishes it.
	two := outboxTestJob()
	two.Deliveries = append(two.Deliveries, OutboundDelivery{Recipients: []string{"other@outside.test"}, Raw: two.Deliveries[0].Raw})
	two.Deliveries[0].Recipients = two.Deliveries[0].Recipients[:1]
	for _, follow := range []string{"uncertain", "quarantined", "accepted"} {
		t.Run("follow-on "+follow, func(t *testing.T) {
			s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
			must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, two))
			whole := outboxCiphertext(t, s, outboxTestID)
			_, claim, err := s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 0, outboxGeneration)
			must(t, err)
			must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 0, claim, nil))
			_, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
			must(t, err)
			if outboxCiphertext(t, s, outboxTestID) != whole {
				t.Fatal("compacted with a queued follow-on")
			}
			if follow == "quarantined" {
				must(t, s.QuarantineOutbound(ctx, outboxTestID, 1))
			} else {
				_, claim, err = s.ClaimOutbound(ctx, outboxMaster, outboxTestID, 1, outboxGeneration)
				must(t, err)
				var submission error
				if follow == "uncertain" {
					submission = mailmsg.ErrSMTPAcceptanceUncertain
				}
				must(t, s.CompleteOutbound(ctx, outboxMaster, outboxTestID, 1, claim, submission))
			}
			outboxUsage(t, s)
			if compacted := outboxCiphertext(t, s, outboxTestID) < whole; compacted != (follow == "accepted") {
				t.Fatal("compaction", follow, compacted)
			}
			if _, _, sent, err := s.ReadOutbound(ctx, outboxMaster, outboxTestID); err != nil || sent == 0 {
				t.Fatal("read", sent, err)
			}
			must(t, s.QueueOutbound(ctx, outboxMaster, outboxTestID, two))
		})
	}
}
