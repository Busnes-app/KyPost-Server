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
	must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, mailmsg.ErrSMTPAcceptedThenFailed))
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
			must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, tc.submission))
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
					must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, tc.submission))
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
	if err = s.CompleteOutbound(ctx, outboxTestID, 0, claim, &textproto.Error{Code: 451, Msg: "retry"}); err == nil {
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
	must(t, s.CompleteOutbound(ctx, outboxTestID, 0, claim, nil))
	_, err = s.FileOutboundSent(ctx, outboxMaster, outboxTestID)
	must(t, err)
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage").Scan(&afterCount, &afterUsed))
	if afterCount != count || afterUsed != used {
		t.Fatal("Sent did not consume its reservation")
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
	relay := mailmsg.DomainRelay{Version: 1, Generation: outboxGeneration, Domain: "example.test", Issuer: testOwner.Issuer, Host: "smtp.provider.test", Port: 465, Username: "operator", Password: "secret"}
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
