//go:build linux

package processor

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// An unchanged native mailbox is passed over after a pass that left nothing
// pending; new mail, quietRecheck and poll-now each bring it back.
func TestQuietMailboxSkippedUntilItChanges(t *testing.T) {
	p, _, u, m := nativeMailboxFixture(t)
	polled, appendTo := quietHarness(t, p, u)

	appendTo("")
	for range 3 { // process, then passes until labelling's own writes settle
		p.tick()
	}
	if n := polled(p.tick); n != 0 {
		t.Fatalf("unchanged mailboxes: %d polled, want both passed over", n)
	}
	appendTo(m.ID)
	if n := polled(p.tick); n != 1 {
		t.Fatalf("after mail to the extra mailbox: %d polled, want 1", n)
	}
	if n := polled(p.TriggerNow); n != 2 {
		t.Fatalf("poll now: %d polled, want every mailbox", n)
	}
	p.quietMu.Lock()
	for k, q := range p.quiet {
		q.at = time.Now().Add(-quietRecheck)
		p.quiet[k] = q
	}
	p.quietMu.Unlock()
	if n := polled(p.tick); n != 2 {
		t.Fatalf("after quietRecheck: %d polled, want every mailbox", n)
	}
}

// Deferred mail keeps its mailbox in every pass.
func TestDeferringMailboxIsNeverQuiet(t *testing.T) {
	p, _, u, _ := nativeMailboxFixture(t)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = true
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 0, 0 // the classifier's budget defers it
	polled, appendTo := quietHarness(t, p, u)
	appendTo("")
	for range 3 {
		p.tick()
	}
	if n := polled(p.tick); n != 1 {
		t.Fatalf("%d polled, want the primary with deferred mail polled and the empty extra passed over", n)
	}
}

func quietHarness(t *testing.T, p *Poller, u users.User) (polled func(func()) int, appendTo func(string)) {
	t.Helper()
	ctx := context.Background()
	var logs bytes.Buffer
	logger, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	p.log = logger
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	polled = func(tick func()) int {
		t.Helper()
		logs.Reset()
		tick()
		return strings.Count(logs.String(), "user poll tick summary")
	}
	appendTo = func(mailboxID string) {
		t.Helper()
		a, _, err := p.nativeMailboxAssignment(ctx, u.ID, mailboxID)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(p.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, err := store.Append(ctx, "INBOX", bytes.NewBufferString("From: sender@outside.test\r\nTo: "+a.Address+"\r\nSubject: hi\r\n\r\nbody"), false); err != nil {
			t.Fatal(err)
		}
	}

	return polled, appendTo
}
