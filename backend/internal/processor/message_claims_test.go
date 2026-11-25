package processor

import (
	"context"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// pausingMailbox stalls its first label write: a daemon paused mid-message.
type pausingMailbox struct {
	scriptedMailbox
	entered, leave chan struct{}
}

func (m *pausingMailbox) ApplyLabel(ctx context.Context, id, label string) error {
	if m.entered != nil {
		close(m.entered)
		m.entered = nil
		<-m.leave
	}
	return m.scriptedMailbox.ApplyLabel(ctx, id, label)
}

var claimTestMessages = []imapadapter.Message{{ID: "7", Sender: "a@example.com", Subject: "once", Body: "x"}}

// twoHostPollers is two daemons over one state, claims on: A's label write pauses until
// leave is closed.
func twoHostPollers(t *testing.T) (a, b *Poller, u users.User, mailA *pausingMailbox, mailB *scriptedMailbox, entered chan struct{}) {
	t.Helper()
	prev := claimsOn
	claimsOn = func(string) bool { return true }
	t.Cleanup(func() { claimsOn = prev })
	entered = make(chan struct{})
	mailA = &pausingMailbox{scriptedMailbox: scriptedMailbox{msgs: claimTestMessages}, entered: entered, leave: make(chan struct{})}
	a, u = newTickTestPoller(t, mailA)
	mailB = &scriptedMailbox{msgs: claimTestMessages}
	b = &Poller{
		cfg: a.cfg, log: a.log, users: a.users, health: a.health, stateDir: a.stateDir, configDir: a.configDir,
		stores: map[string]*state.Store{}, mailClients: map[string]*mailClientEntry{}, mailCaches: map[string]*mailcache.Store{},
		rulesStores: map[string]*rules.Store{}, sendAsStores: map[string]*sendas.Store{}, contactsStores: map[string]*contacts.Store{},
		rate: map[string][]time.Time{}, newMailClient: func(string, string) imapadapter.Client { return mailB },
	}
	global := t.TempDir()
	for _, p := range []*Poller{a, b} {
		st, err := state.New(global)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		p.globalStore = st
	}
	return a, b, u, mailA, mailB, entered
}

func decisionCount(t *testing.T, p *Poller, u users.User) int {
	t.Helper()
	d, err := tickStore(t, p, u).DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	return len(d)
}

// A daemon paused mid-message past its role's grace resumes beside the new owner (MH4). In
// table mode the new owner leaves the message to the paused one, so it is labelled, recorded
// and pushed once; the next tick sees it processed.
func TestTwoHostsHandleAMessageOnce(t *testing.T) {
	a, b, u, mailA, mailB, entered := twoHostPollers(t)
	done := make(chan error, 1)
	go func() { done <- a.tickUser(u, time.Time{}) }()
	<-entered
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(mailB.labelled) != 0 {
		t.Fatalf("the new owner handled a message the paused one is on: %v", mailB.labelled)
	}
	close(mailA.leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := decisionCount(t, b, u); len(mailA.labelled) != 1 || len(mailB.labelled) != 0 || n != 1 {
		t.Fatalf("labelled %v and %v with %d decisions, want once", mailA.labelled, mailB.labelled, n)
	}
}

// A host frozen past its claim's TTL resumes after another host took the claim over: the
// taker handles the message, and the resumed host records and pushes nothing.
func TestExpiredClaimHolderRecordsNothing(t *testing.T) {
	prev := messageClaimTTL
	messageClaimTTL = 200 * time.Millisecond
	t.Cleanup(func() { messageClaimTTL = prev })
	a, b, u, mailA, mailB, entered := twoHostPollers(t)
	done := make(chan error, 1)
	go func() { done <- a.tickUser(u, time.Time{}) }()
	<-entered
	time.Sleep(300 * time.Millisecond)
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(mailB.labelled) != 1 || decisionCount(t, b, u) != 1 {
		t.Fatalf("the taker of an expired claim did not handle the message: %v", mailB.labelled)
	}
	close(mailA.leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := decisionCount(t, b, u); n != 1 {
		t.Fatalf("%d decisions: the resumed host recorded a message another host took over", n)
	}
}

// A host that read the message unprocessed, then paused while another host finished it and
// released its claim, reads it again under its claim and leaves it.
func TestClaimRereadsProcessedState(t *testing.T) {
	a, b, u, mailA, mailB, _ := twoHostPollers(t)
	close(mailA.leave)
	prev := beforeClaim
	beforeClaim = func() {
		beforeClaim = func() {}
		if err := a.tickUser(u, time.Time{}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeClaim = prev })
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := decisionCount(t, b, u); len(mailA.labelled) != 1 || len(mailB.labelled) != 0 || n != 1 {
		t.Fatalf("labelled %v and %v with %d decisions, want once", mailA.labelled, mailB.labelled, n)
	}
}

// The same takeover through a filter rule that does not stop: A freezes inside the rule's
// keyword action, B takes the message over and handles it, and A, resumed, writes neither the
// rule's decision nor its own record.
func TestExpiredClaimHolderRecordsNoRuleDecision(t *testing.T) {
	prev := messageClaimTTL
	messageClaimTTL = 200 * time.Millisecond
	t.Cleanup(func() { messageClaimTTL = prev })
	a, b, u, mailA, mailB, entered := twoHostPollers(t)
	seedRule(t, a, u, rules.Rule{
		Name:    "tag and carry on",
		Enabled: true,
		Match:   rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "subject", Comparator: "contains", Value: "once"}}},
		Actions: []rules.Action{{Type: "keyword", Value: "VIP"}},
	})
	done := make(chan error, 1)
	go func() { done <- a.tickUser(u, time.Time{}) }()
	<-entered
	time.Sleep(300 * time.Millisecond)
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	want := decisionCount(t, b, u) // the rule's decision and the record, B's
	if want != 2 || len(mailB.labelled) == 0 {
		t.Fatalf("the taker wrote %d decisions, labelled %v; want the rule's and the record", want, mailB.labelled)
	}
	close(mailA.leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := decisionCount(t, b, u); n != want {
		t.Fatalf("%d decisions, want %d: the resumed host wrote for a message another host took over", n, want)
	}
}

// And through the phishing verdict, written before classification: A freezes applying the
// phishing keyword, B takes the message over, and A, resumed, writes no verdict row.
func TestExpiredClaimHolderRecordsNoPhishingVerdict(t *testing.T) {
	prev := messageClaimTTL
	messageClaimTTL = 200 * time.Millisecond
	t.Cleanup(func() { messageClaimTTL = prev })
	a, b, u, mailA, mailB, entered := twoHostPollers(t)
	phish := []imapadapter.Message{{ID: "7", Sender: "a@example.com", Subject: "pair", Body: "Confirm: kypost://native-pair?sub=x"}}
	mailA.msgs, mailB.msgs = phish, phish
	done := make(chan error, 1)
	go func() { done <- a.tickUser(u, time.Time{}) }()
	<-entered
	time.Sleep(300 * time.Millisecond)
	if err := b.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	want := decisionCount(t, b, u)
	if want < 2 {
		t.Fatalf("the taker wrote %d decisions; want its verdict and its record", want)
	}
	close(mailA.leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := decisionCount(t, b, u); n != want {
		t.Fatalf("%d decisions, want %d: the resumed host wrote a verdict for a message another host took over", n, want)
	}
}
