package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

func setSendQuota(s *Server, messagesPerHour, recipientsPerDay int) {
	s.cfgMu.Lock()
	s.cfg.SendQuota = config.SendQuota{MessagesPerHour: messagesPerHour, RecipientsPerDay: recipientsPerDay}
	s.cfgMu.Unlock()
}

func sendNow(f *scheduledFixture) int {
	return f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "cc": "carol@example.com", "subject": "now", "body": "hi", "mode": "plain"}).Code
}

func TestSendQuotaRefusesImmediateSend(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 2, 100)
	for i := range 2 {
		if code := sendNow(f); code != http.StatusOK {
			t.Fatalf("send %d inside the quota: %d", i, code)
		}
	}
	rec := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "subject": "now", "body": "hi", "mode": "plain"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("send over the quota: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if _, data := f.smtp.counts(); data != 2 {
		t.Fatalf("%d messages reached SMTP, want 2", data)
	}
}

func TestSendQuotaCountsRecipients(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 100, 3)
	if code := sendNow(f); code != http.StatusOK {
		t.Fatal("two recipients of three", code)
	}
	if code := sendNow(f); code != http.StatusTooManyRequests {
		t.Fatal("four recipients of three", code)
	}
}

func TestSendQuotaUserOverride(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 100, 100)
	if _, err := f.srv.users.SetSendQuota(f.userID, &config.SendQuota{MessagesPerHour: 1, RecipientsPerDay: 100}); err != nil {
		t.Fatal(err)
	}
	if code := sendNow(f); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := sendNow(f); code != http.StatusTooManyRequests {
		t.Fatal("override ignored", code)
	}
}

func TestSendQuotaRefusesClientEncryptedSend(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	u := convertedUser(t, f.srv)
	f.useAccount(t, u.ID)
	setSendQuota(f.srv, 1, 100)
	if code := sendNow(f); code != http.StatusOK {
		t.Fatal(code)
	}
	delivery := scheduledPGPDelivery("alice@example.com", time.Now())
	generation := uint64(1)
	rec := f.post("/api/mail/send-pgp", clientEncryptedSendRequest{
		Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: delivery}},
		To:         []string{"bob@example.com"}, SentCopy: delivery, SentCopyEncrypted: true, MaterialGeneration: &generation,
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("encrypted send over the quota: %d %s", rec.Code, rec.Body)
	}
	if _, data := f.smtp.counts(); data != 1 {
		t.Fatalf("%d messages reached SMTP, want 1", data)
	}
}

// A scheduled send over the quota waits for it, then returns to Drafts.
func TestSendQuotaHoldsThenReturnsScheduledSend(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	id := f.schedule(t, at, nil)
	setSendQuota(f.srv, 1, 100)
	if _, err := f.srv.chargeSend(context.Background(), "earlier", f.userID, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.srv.sweepScheduledSends(ctx, clockAt(at))
	if row := f.row(t, id); row.Status != "scheduled" {
		t.Fatalf("over-quota send not held for retry: %+v", row)
	}
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryWindow+time.Minute)))
	assertReturned(t, f, id, reasonQuota)
}

// Retries of one scheduled send are charged once.
func TestSendQuotaChargesScheduledRetryOnce(t *testing.T) {
	f := newScheduledFixture(t, "451 4.3.0 try later")
	at := sendTime()
	id := f.schedule(t, at, nil)
	setSendQuota(f.srv, 1, 100)
	ctx := context.Background()
	f.srv.sweepScheduledSends(ctx, clockAt(at))
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryDelay)))
	if _, data := f.smtp.counts(); data != 2 {
		t.Fatalf("retry was refused by its own charge: %d attempts, row %+v", data, f.row(t, id))
	}
}

func TestSendQuotaConfigRejectsZero(t *testing.T) {
	q := config.Default().SendQuota
	if !q.Valid() {
		t.Fatal("default quota is not valid")
	}
	q.MessagesPerHour = 0
	if q.Valid() {
		t.Fatal("zero quota accepted")
	}
}

// Send-as verification mails a third party, so it spends the quota too.
func TestSendQuotaChargesSendAsProbe(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 1, 100)
	if code := sendNow(f); code != http.StatusOK {
		t.Fatal(code)
	}
	rec := f.post("/api/mail/send-as", map[string]any{"email": "someone@elsewhere.example"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("probe over the quota: %d %s", rec.Code, rec.Body)
	}
	if _, data := f.smtp.counts(); data != 1 {
		t.Fatalf("%d messages reached SMTP, want 1", data)
	}
}

// More recipients than a day allows can never fit, so it is not a 429.
func TestSendQuotaTooLargeIsNotRetryable(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 100, 1)
	if code := sendNow(f); code != http.StatusUnprocessableEntity {
		t.Fatal("two recipients against a daily quota of one:", code)
	}
}

// A refusal the client answers by sending again must not spend the quota.
func TestSendQuotaSkipsRefusedSend(t *testing.T) {
	srv, userID := newPickupGateServer(t)
	setSendQuota(srv, 1, 100)
	for range 2 {
		if rec := sendEncrypted(t, srv, false); rec.Code != http.StatusConflict {
			t.Fatalf("keyless refusal: %d %s", rec.Code, rec.Body)
		}
	}
	if _, err := srv.chargeSend(context.Background(), "after", userID, 1, time.Now()); err != nil {
		t.Fatal("refusals were charged:", err)
	}
}
