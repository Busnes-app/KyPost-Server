package processor

import (
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// The rate limit rations LLM calls. With AI sorting off none is made, so the
// budget must defer nothing: every message in the pass is handled.
func TestDisabledAISortingSpendsNoLLMBudget(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "a@example.com", Subject: "one", Body: "first"},
		{ID: "2", Sender: "b@example.com", Subject: "two", Body: "second"},
	}}
	p, u := newTickTestPoller(t, mail)
	p.DisableAISorting()
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 0, 0

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	decisions, err := tickStore(t, p, u).DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("handled %d of 2 messages (%v), want both: no LLM call means no budget to spend", len(decisions), decisions)
	}
}

// An encrypted message has nothing to classify and takes the fallback label,
// so it is not deferred for a budget it would never use.
func TestEncryptedMailSpendsNoLLMBudget(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{{ID: "1", Sender: "a@example.com", Subject: "sealed", PGPEncrypted: true}}}
	p, u := newTickTestPoller(t, mail)
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 0, 0

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	decisions, err := tickStore(t, p, u).DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 {
		t.Fatalf("handled %d of 1 encrypted message, want it handled without LLM budget", len(decisions))
	}
}
