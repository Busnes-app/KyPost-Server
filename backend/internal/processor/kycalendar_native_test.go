//go:build linux

package processor

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// Native mailboxes take the same inbound path as IMAP ones: an aligned
// invitation in the primary or an extra mailbox is forwarded once, under
// its own event ID, and recorded as provenance for a later REPLY.
func TestNativeMailboxForwardsInvitations(t *testing.T) {
	ctx := context.Background()
	p, _, u, m := nativeMailboxFixture(t)
	if u.SSOSub == "" || u.SSOIssuer == "" {
		t.Fatalf("native account without an SSO link: %+v", u)
	}
	stubAlignment(t, map[string]string{"From: alice@example.com": "alice@example.com"})
	forwarder, fake := newForwardPoller(t)
	p.SetKyCalendar(forwarder.itip.client)
	on := true
	if err := config.UpdateUserSettings(p.userSettingsPath(u.ID), func(s *config.UserSettings) error { s.KyCalendar = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", m.ID} {
		a, _, err := p.nativeMailboxAssignment(ctx, u.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(p.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, "INBOX", bytes.NewReader(inviteMail("alice@example.com", forwardICS)), false); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
	}
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	p.tick()
	p.tick() // seen messages are not forwarded again
	if len(fake.bodies) != 2 || fake.ids[0] == fake.ids[1] {
		t.Fatalf("forwards %d %v, want one per mailbox", len(fake.bodies), fake.ids)
	}
	for i, b := range fake.bodies {
		if b["sub"] != u.SSOSub || b["issuer"] != u.SSOIssuer || b["from"] != "alice@example.com" || b["ics"] != forwardICS || !strings.HasPrefix(fake.ids[i], "kypost-inbound-") {
			t.Fatalf("forward %d: %v", i, b)
		}
	}
	if seen, err := kycalendar.OpenInvitations(p.userStateDir(u.ID)).Has("e1@example.com", "alice@example.com"); err != nil || !seen {
		t.Fatal("provenance not recorded", seen, err)
	}
}
