package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/logging"
)

// Exercise real SMTP through both authenticated send routes. A correction
// must survive proxies that replace 502 responses with their own HTML.
func TestMailSendInvalidRecipient(t *testing.T) {
	for _, tc := range []struct {
		name, reply, detail string
		status              int
	}{
		{"mailbox", "RCPT 550 5.1.1 user unknown", "the mailbox does not exist", 422},
		{"quoted domain", `RCPT 550 "5.1.2 <bob@example.com>: Domain not found"`, "the recipient domain could not be found", 422},
		{"syntax", "RCPT 553 5.1.3 bad destination mailbox syntax", "the recipient address is invalid", 422},
		{"untrusted reply", "RCPT 550 5.1.2 secret-password <script> other-recipient@example.com", "the recipient domain could not be found", 422},
		{"policy", "RCPT 550 5.7.1 blocked by policy", "", 502},
		{"temporary", "RCPT 450 4.1.1 temporary lookup failure", "", 502},
		{"inconsistent temporary", "RCPT 450 5.1.2 inconsistent reply", "", 502},
		{"no enhanced code", "RCPT 550 recipient refused", "", 502},
		{"similar code", "RCPT 550 5.1.20 unknown error", "", 502},
		{"DATA rejection", "550 5.1.2 failure after DATA", "", 502},
	} {
		for _, route := range []string{"/api/mail/send", "/api/mail/send-pgp"} {
			t.Run(tc.name+route, func(t *testing.T) {
				f := newScheduledFixture(t, tc.reply)
				var logs bytes.Buffer
				var err error
				f.srv.logger, err = logging.NewWithOutput(&logs)
				if err != nil {
					t.Fatal(err)
				}
				clearMustChangePassword(t, f.srv, f.userID)
				var request any = map[string]any{"to": "bob@example.com", "subject": "keep me", "body": "hello"}
				if route == "/api/mail/send-pgp" {
					request = clientEncryptedSendRequest{
						Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: scheduledPGPDelivery("alice@example.com", time.Now())}},
						To:         []string{"bob@example.com"},
					}
				}
				rec := f.post(route, request)
				if rec.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
				}
				if tc.status != http.StatusUnprocessableEntity {
					return
				}
				var body struct{ Code, Error string }
				if rec.Header().Get("Content-Type") != "application/json" || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
					t.Fatalf("expected JSON refusal: %s", rec.Body.String())
				}
				if body.Code != "invalid_recipient" || !strings.Contains(body.Error, "bob@example.com") || !strings.Contains(body.Error, tc.detail) || !strings.Contains(body.Error, "Check the recipient address") {
					t.Fatalf("unhelpful refusal: %s", rec.Body.String())
				}
				for _, private := range []string{"secret-password", "<script>", "other-recipient@example.com"} {
					if strings.Contains(body.Error, private) {
						t.Fatalf("provider reply leaked: %s", body.Error)
					}
				}
				if _, data := f.smtp.counts(); data != 0 {
					t.Fatal("sent DATA after a recipient was rejected")
				}
				if !strings.Contains(logs.String(), "mail send failed") {
					t.Fatal("missing diagnostic failure log")
				}
				for _, private := range []string{"bob@example.com", "secret-password", "other-recipient@example.com"} {
					if strings.Contains(logs.String(), private) {
						t.Fatalf("correspondence or provider reply reached instance log: %s", logs.String())
					}
				}
			})
		}
	}
}
