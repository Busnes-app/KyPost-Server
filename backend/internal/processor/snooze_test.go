package processor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
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
	return msg.MessageID, f.snoozeRaw(t, subject, msg.MessageID, msg.Build(), wakeAt)
}

func (f snoozeFixture) snoozeRaw(t *testing.T, delivery, messageID string, raw []byte, wakeAt int64) int {
	t.Helper()
	id, err := f.box.Import(context.Background(), mailbox.Receipt{Gateway: "test", Delivery: delivery, Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: "one@example.test", Generation: 1}}}, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Snooze(messageID, wakeAt); err != nil {
		t.Fatal(err)
	}
	if err := f.uc.mail.ApplyInboxAction(context.Background(), strconv.FormatInt(id, 10), "move", "INBOX", imapadapter.SnoozedFolder); err != nil {
		t.Fatal(err)
	}
	return int(id)
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

// A wake push goes backend -> relay -> FCM/APNs in cleartext, so encrypted
// mail must stay generic even with content previews on, as on arrival.
func TestWakeSnoozedKeepsEncryptedMailOutOfPush(t *testing.T) {
	f := newSnoozeFixture(t)
	f.uc.settings.ContentPreview = true
	raw := []byte("From: sender@outside.test\r\nTo: one@example.test\r\nSubject: secret-outer-subject\r\n" +
		"Message-ID: <enc@outside.test>\r\nDate: Mon, 12 Oct 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n\r\n-----BEGIN PGP MESSAGE-----\r\n\r\nwcBMA0Zx\r\n=AAAA\r\n-----END PGP MESSAGE-----\r\n" +
		"--b--\r\n")
	f.snoozeRaw(t, "enc", "<enc@outside.test>", raw, time.Now().Unix()-1)

	f.p.wakeSnoozed(context.Background(), f.uc)

	queued, _, err := f.st.PullNotificationsAfterStrict("one", 0)
	if err != nil || len(queued) != 1 {
		t.Fatalf("notifications = %d, %v; want 1", len(queued), err)
	}
	n := queued[0]
	if n.Title != "KyPost" || n.Body != "You have a new email." {
		t.Fatalf("encrypted wake push = %q / %q; want the generic text", n.Title, n.Body)
	}
	for k, v := range n.Data {
		if strings.Contains(v, "secret-outer-subject") || strings.Contains(v, "sender@outside.test") || k == "Keywords" {
			t.Fatalf("encrypted wake push leaked %s=%q", k, v)
		}
	}
}

// The privacy check must not turn every wake generic: plain mail still previews.
func TestWakeSnoozedPreviewsPlainMail(t *testing.T) {
	f := newSnoozeFixture(t)
	f.uc.settings.ContentPreview = true
	f.snooze(t, "plain-subject", time.Now().Unix()-1)
	f.p.wakeSnoozed(context.Background(), f.uc)
	queued, _, err := f.st.PullNotificationsAfterStrict("one", 0)
	if err != nil || len(queued) != 1 || queued[0].Body != "plain-subject" {
		t.Fatalf("plain wake push = %+v, %v; want the subject as body", queued, err)
	}
}

// flakyMail wraps the native client as an IMAP-identified client (the embedded
// interface hides MailSourceIdentity) and injects lookup faults.
type flakyMail struct {
	imapadapter.Client
	failList   map[string]int // folder -> remaining ListOverviews failures
	hideHeader int            // UID whose headers are never returned
}

func (f *flakyMail) ListOverviews(ctx context.Context, folder string, n int) ([]imapadapter.Overview, error) {
	if f.failList[folder] > 0 {
		f.failList[folder]--
		return nil, errors.New("injected lookup failure")
	}
	return f.Client.ListOverviews(ctx, folder, n)
}

func (f *flakyMail) FetchHeaderFields(ctx context.Context, folder string, uids []int, fields ...string) (map[int][]string, error) {
	got, err := f.Client.FetchHeaderFields(ctx, folder, uids, fields...)
	delete(got, f.hideHeader)
	return got, err
}

func inFolder(t *testing.T, c imapadapter.Client, folder string, uid int) bool {
	t.Helper()
	_, ok, err := imapadapter.OverviewByID(context.Background(), c, folder, uid)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Headers a batch did not return are unknown, not absent: the snooze stays.
func TestWakeSnoozedKeepsRowWhenHeadersAreIncomplete(t *testing.T) {
	f := newSnoozeFixture(t)
	id, uid := f.snooze(t, "budget", time.Now().Unix()-1)
	f.uc.mail = &flakyMail{Client: f.uc.mail, hideHeader: uid}

	f.p.wakeSnoozed(context.Background(), f.uc)

	if rows := mustSnoozes(t, f.st); len(rows) != 1 || rows[0].MessageID != id {
		t.Fatalf("snoozes = %+v; an unread header must not drop the row", rows)
	}
	if !inFolder(t, f.uc.mail, imapadapter.SnoozedFolder, uid) {
		t.Fatal("message left Snoozed without a wake")
	}
}

// A failed lookup, however overdue the snooze, is retried on the next tick.
func TestWakeSnoozedRecoversOverdueAfterFailure(t *testing.T) {
	f := newSnoozeFixture(t)
	_, uid := f.snooze(t, "overdue", time.Now().Add(-8*24*time.Hour).Unix())
	flaky := &flakyMail{Client: f.uc.mail, failList: map[string]int{imapadapter.SnoozedFolder: 1}}
	f.uc.mail = flaky

	f.p.wakeSnoozed(context.Background(), f.uc)
	if len(mustSnoozes(t, f.st)) != 1 {
		t.Fatal("a failed lookup dropped the overdue snooze")
	}
	f.p.wakeSnoozed(context.Background(), f.uc)
	if !inFolder(t, f.uc.mail, "INBOX", uid) {
		t.Fatal("the overdue message did not return to INBOX after recovery")
	}
}

// IMAP pins resolve on a later tick when the post-move lookup fails, keeping
// the original wake time.
func TestWakeSnoozedResolvesPinOnLaterTick(t *testing.T) {
	f := newSnoozeFixture(t)
	id, uid := f.snooze(t, "pin", time.Now().Unix()-1)
	f.uc.mail = &flakyMail{Client: f.uc.mail, failList: map[string]int{"INBOX": 1}}

	f.p.wakeSnoozed(context.Background(), f.uc)
	if w, _ := f.st.Woken(); len(w) != 0 {
		t.Fatalf("pinned despite a failed lookup: %+v", w)
	}
	pending, _ := f.st.Unpinned()
	if len(pending) != 1 {
		t.Fatalf("Unpinned = %+v; want the woken message awaiting its UID", pending)
	}
	wokeAt := pending[0].WokenAt

	f.p.wakeSnoozed(context.Background(), f.uc)
	w, _ := f.st.Woken()
	if len(w) != 1 || w[0].MessageID != id || w[0].InboxID != strconv.Itoa(uid) || w[0].WokenAt != wokeAt {
		t.Fatalf("Woken = %+v; want %s pinned at %d with the first wake time %d", w, id, uid, wokeAt)
	}
}

// Native mail woken before the poller processed it is notified once, by
// arrival processing; mail it already processed is notified by the wake.
func TestWakeSnoozedLeavesUnprocessedNativeMailToArrival(t *testing.T) {
	f := newSnoozeFixture(t)
	if err := f.st.SetCheckpoint("0"); err != nil {
		t.Fatal(err)
	}
	_, fresh := f.snooze(t, "unprocessed", time.Now().Unix()-1)
	_, old := f.snooze(t, "processed", time.Now().Unix()-1)
	if err := f.st.MarkProcessed(strconv.Itoa(old)); err != nil {
		t.Fatal(err)
	}

	f.p.wakeSnoozed(context.Background(), f.uc)

	queued, _, err := f.st.PullNotificationsAfterStrict("one", 0)
	if err != nil || len(queued) != 1 || !strings.HasSuffix(queued[0].Data["messageId"], ":"+strconv.Itoa(old)) {
		t.Fatalf("wake notifications = %+v, %v; want only the processed one", queued, err)
	}
	if !inFolder(t, f.uc.mail, "INBOX", fresh) {
		t.Fatal("unprocessed message did not wake")
	}
	// Arrival still sees it: above the checkpoint and unread.
	unread, _, err := f.uc.mail.ListUnreadInbox(context.Background(), "0")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range unread {
		found = found || m.ID == strconv.Itoa(fresh)
	}
	if !found {
		t.Fatalf("arrival poll does not see the unprocessed woken message: %+v", unread)
	}
}
