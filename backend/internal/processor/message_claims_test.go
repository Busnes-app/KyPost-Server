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

// A daemon paused mid-message past its role's grace resumes beside the new owner (MH4). In
// table mode the new owner leaves the message to the paused one, so it is labelled, recorded
// and pushed once; the next tick sees it processed.
func TestTwoHostsHandleAMessageOnce(t *testing.T) {
	prev := claimsOn
	claimsOn = func(string) bool { return true }
	t.Cleanup(func() { claimsOn = prev })
	msgs := []imapadapter.Message{{ID: "7", Sender: "a@example.com", Subject: "once", Body: "x"}}
	mailA := &pausingMailbox{scriptedMailbox: scriptedMailbox{msgs: msgs}, entered: make(chan struct{}), leave: make(chan struct{})}
	entered := mailA.entered
	a, u := newTickTestPoller(t, mailA)
	mailB := &scriptedMailbox{msgs: msgs}
	b := &Poller{
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
	decisions, err := tickStore(t, b, u).DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailA.labelled) != 1 || len(mailB.labelled) != 0 || len(decisions) != 1 {
		t.Fatalf("labelled %v and %v with %d decisions, want once", mailA.labelled, mailB.labelled, len(decisions))
	}
}
