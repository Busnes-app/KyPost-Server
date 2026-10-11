package processor

import (
	"context"
	"strings"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// namespacedMailbox issues "<ns>:<uid>" checkpoints the way the IMAP adapter
// does: a checkpoint from another namespace restarts at UID 0, a bare one is
// trusted.
type namespacedMailbox struct {
	scriptedMailbox
	ns string
}

func (m *namespacedMailbox) ListUnreadInbox(ctx context.Context, checkpoint string) ([]imapadapter.Message, string, error) {
	prevNS, uid, found := strings.Cut(checkpoint, ":")
	if !found {
		uid = prevNS
	} else if prevNS != m.ns {
		uid = ""
	}
	msgs, next, err := m.scriptedMailbox.ListUnreadInbox(ctx, uid)
	if next == "" {
		next = "0"
	}
	return msgs, m.ns + ":" + next, err
}

func uidValidityTick(t *testing.T, ns, checkpoint string) (*namespacedMailbox, *state.Store) {
	t.Helper()
	mail := &namespacedMailbox{ns: ns, scriptedMailbox: scriptedMailbox{msgs: []imapadapter.Message{{ID: "3", Subject: "new", Sender: "a@example.com"}}}}
	p, u := newTickTestPoller(t, mail)
	store := tickStore(t, p, u)
	if err := store.SetCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "3"} {
		if err := store.MarkProcessed(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecordDeferral("2"); err != nil {
		t.Fatal(err)
	}
	if err := p.tickUser(u, time.Now()); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	return mail, store
}

// UIDs restarted below the old checkpoint, and UID 3 is still in the processed
// set from the old mailbox: the new message 3 must be processed anyway.
func TestTickUser_UIDValidityChangeProcessesReusedUIDs(t *testing.T) {
	mail, store := uidValidityTick(t, "B", "A:500")
	if len(mail.labelled) != 1 || mail.labelled[0] != "3:Primary" {
		t.Fatalf("labelled = %v, want [3:Primary]", mail.labelled)
	}
	if seenForTest(t, store, "1") || !seenForTest(t, store, "3") {
		t.Fatal("processed set should hold only the new namespace's message 3")
	}
	if n, err := store.DeferralAttempts("2"); err != nil || n != 0 {
		t.Fatalf("stale deferral survived: %d, %v", n, err)
	}
	if got := checkpointOf(t, store); got != "B:3" {
		t.Fatalf("checkpoint = %q, want B:3", got)
	}
}

func TestTickUser_UnchangedUIDValidityKeepsProcessedSet(t *testing.T) {
	for _, cp := range []string{"A:2", "2"} { // current and old checkpoint format
		mail, store := uidValidityTick(t, "A", cp)
		if len(mail.labelled) != 0 {
			t.Fatalf("%s: already-processed message relabelled: %v", cp, mail.labelled)
		}
		if !seenForTest(t, store, "1") || !seenForTest(t, store, "3") {
			t.Fatalf("%s: processed set was cleared", cp)
		}
		if n, _ := store.DeferralAttempts("2"); n != 1 {
			t.Fatalf("%s: deferral cleared", cp)
		}
		if got := checkpointOf(t, store); got != "A:3" {
			t.Fatalf("%s: checkpoint = %q, want A:3", cp, got)
		}
	}
}
