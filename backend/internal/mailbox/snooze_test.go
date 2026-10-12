package mailbox

import (
	"context"
	"errors"
	"strconv"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// Snooze runs through the shared client interface; these pin the native half.
func TestNativeSnoozeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	msg := mailmsg.Message{From: "sender@outside.test", To: []string{"alice@example.test"}, Subject: "later", Body: "b"}.Stamp()
	uid := importClient(t, s, "snooze", msg.Build())
	id := strconv.Itoa(uid)

	got, err := imapadapter.MessageIDOf(ctx, c, "", uid)
	must(t, err)
	if got != msg.MessageID {
		t.Fatalf("MessageIDOf = %q, want %q", got, msg.MessageID)
	}

	// Moving to a missing top-level folder creates it, as IMAP does.
	must(t, c.ApplyInboxAction(ctx, id, "move", "INBOX", imapadapter.SnoozedFolder))
	refs, err := imapadapter.MessageIDRefs(ctx, c, imapadapter.SnoozedFolder, 100)
	must(t, err)
	if refs[msg.MessageID] != uid {
		t.Fatalf("MessageIDRefs = %+v, want %s -> %d", refs, msg.MessageID, uid)
	}

	must(t, c.ApplyInboxAction(ctx, id, "move", imapadapter.SnoozedFolder, "INBOX"))
	ov, ok, err := imapadapter.OverviewByID(ctx, c, "INBOX", uid)
	must(t, err)
	if !ok || ov.UID != uid || ov.Subject != "later" {
		t.Fatalf("OverviewByID = %+v, %v", ov, ok)
	}
	if _, ok, _ := imapadapter.OverviewByID(ctx, c, "INBOX", uid+1); ok {
		t.Fatal("OverviewByID found a message that does not exist")
	}

	// A nested target still needs its parent.
	if err := c.ApplyInboxAction(ctx, id, "move", "INBOX", "Nope/Child"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("move under missing parent = %v, want ErrNotFound", err)
	}
}

func TestMessageIDOfRefusesMissingHeader(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	uid := importClient(t, s, "bare", []byte("From: a@outside.test\r\nTo: alice@example.test\r\nSubject: x\r\n\r\nbody\r\n"))
	if _, err := imapadapter.MessageIDOf(ctx, c, "", uid); !errors.Is(err, imapadapter.ErrNoMessageID) {
		t.Fatalf("MessageIDOf = %v, want ErrNoMessageID", err)
	}
}
