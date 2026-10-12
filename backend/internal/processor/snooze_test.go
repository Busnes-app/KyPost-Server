package processor

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

type snoozeFixture struct {
	p   *Poller
	uc  userCtx
	box *mailbox.Store
	st  *state.Store
}

func newSnoozeFixture(t *testing.T) snoozeFixture {
	t.Helper()
	owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "one", Mailbox: "one"}
	limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	box, err := mailbox.Open(filepath.Join(t.TempDir(), "mailbox"), owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	c, err := mailbox.NewClient(box, "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewNative(filepath.Join(t.TempDir(), "state"), c.MailSourceIdentity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNativeDevice(state.NativeDevice{DeviceID: "one", Platform: "android", PushToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNativeDeliveryMode(state.DeliveryModePull); err != nil {
		t.Fatal(err)
	}
	logger, err := logging.NewWithOutput(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return snoozeFixture{
		p:   &Poller{log: logger},
		uc:  userCtx{id: "one", mail: c, store: st, settings: config.UserNotificationSettings{Mode: "all"}},
		box: box,
		st:  st,
	}
}

// snooze imports a message and snoozes it as the API does.
func (f snoozeFixture) snooze(t *testing.T, subject string, wakeAt int64) (string, int) {
	t.Helper()
	msg := mailmsg.Message{From: "sender@outside.test", To: []string{"one@example.test"}, Subject: subject, Body: "b"}.Stamp()
	id, err := f.box.Import(context.Background(), mailbox.Receipt{Gateway: "test", Delivery: subject, Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: "one@example.test", Generation: 1}}}, bytes.NewReader(msg.Build()))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Snooze(msg.MessageID, wakeAt); err != nil {
		t.Fatal(err)
	}
	if err := f.uc.mail.ApplyInboxAction(context.Background(), strconv.FormatInt(id, 10), "move", "INBOX", imapadapter.SnoozedFolder); err != nil {
		t.Fatal(err)
	}
	return msg.MessageID, int(id)
}

func TestWakeSnoozedReturnsDueMailPinnedAndNotified(t *testing.T) {
	f := newSnoozeFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	dueID, dueUID := f.snooze(t, "due", now-1)
	_, laterUID := f.snooze(t, "later", now+3600)
	goneID, goneUID := f.snooze(t, "gone", now-1)
	// The user moved this one out of Snoozed themselves.
	if err := f.uc.mail.ApplyInboxAction(ctx, strconv.Itoa(goneUID), "move", imapadapter.SnoozedFolder, "Archive"); err != nil {
		t.Fatal(err)
	}

	f.p.wakeSnoozed(ctx, f.uc)

	ov, ok, err := imapadapter.OverviewByID(ctx, f.uc.mail, "INBOX", dueUID)
	if err != nil || !ok || ov.Status != "unread" {
		t.Fatalf("woken message in INBOX = %+v, %v, %v; want unread", ov, ok, err)
	}
	if _, ok, _ := imapadapter.OverviewByID(ctx, f.uc.mail, imapadapter.SnoozedFolder, laterUID); !ok {
		t.Fatal("a message not yet due left Snoozed")
	}
	woken, _ := f.st.Woken()
	if len(woken) != 1 || woken[0].MessageID != dueID || woken[0].InboxID != strconv.Itoa(dueUID) {
		t.Fatalf("Woken = %+v; want the due message pinned at its INBOX id", woken)
	}
	if left, _ := f.st.ListSnoozes(); len(left) != 1 {
		t.Fatalf("ListSnoozes = %+v; want only the later one", left)
	}
	for _, z := range append(woken, mustSnoozes(t, f.st)...) {
		if z.MessageID == goneID {
			t.Fatal("a message the user moved out of Snoozed kept its row")
		}
	}
	queued, _, err := f.st.PullNotificationsAfterStrict("one", 0)
	if err != nil || len(queued) != 1 {
		t.Fatalf("native wake notifications = %d, %v; want 1", len(queued), err)
	}

	// A second tick does nothing more.
	f.p.wakeSnoozed(ctx, f.uc)
	if queued, _, _ := f.st.PullNotificationsAfterStrict("one", 0); len(queued) != 1 {
		t.Fatalf("second tick notified again: %d", len(queued))
	}
}

func mustSnoozes(t *testing.T, st *state.Store) []state.Snooze {
	t.Helper()
	z, err := st.ListSnoozes()
	if err != nil {
		t.Fatal(err)
	}
	return z
}
