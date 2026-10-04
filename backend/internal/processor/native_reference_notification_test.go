package processor

import (
	"io"
	"path/filepath"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestNativeNotificationReferencesPreserveHistoricalGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailbox")
	owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "one", Mailbox: "one"}
	limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	s, err := mailbox.Open(dir, owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := mailbox.NewClient(s, "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	source := c.MailSourceIdentity()
	st, err := state.NewNative(filepath.Join(t.TempDir(), "state"), source)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNativeDevice(state.NativeDevice{DeviceID: "one", Platform: "android", PushToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNativeDeliveryMode(state.DeliveryModePull); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueuePullNotification(state.PullNotification{Title: "historical", Data: map[string]string{"messageId": "1"}, DeviceIDs: []string{"one"}}); err != nil {
		t.Fatal(err)
	}
	logger, err := logging.NewWithOutput(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p := &Poller{log: logger}
	uc := userCtx{id: "one", mail: c, store: st, settings: config.UserNotificationSettings{Mode: "all"}}
	msg := imapadapter.Message{ID: "1", Sender: "sender@outside.test", Subject: "ordinary"}
	p.maybeSendNativePushNotification(uc, msg, "", nil)
	oldReference := "n1:" + s.MessageReferenceGeneration() + ":1"
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Offline generation rotation, not restoration of device authority/hold release.
	if err := mailbox.RotateRestoredMessageReferences(filepath.Join(dir, "mailbox.db"), source); err != nil {
		t.Fatal(err)
	}
	s, err = mailbox.OpenExisting(dir, owner, limits, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err = mailbox.NewClient(s, "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	uc.mail = c
	msg.PGPEncrypted = true
	p.maybeSendNativePushNotification(uc, msg, "", nil)
	freshReference := "n1:" + s.MessageReferenceGeneration() + ":1"
	queued, _, err := st.PullNotificationsAfterStrict("one", 0)
	if err != nil || len(queued) != 3 {
		t.Fatal("new notification creation failed", queued, err)
	}
	for i, expected := range []string{"1", oldReference, freshReference} {
		if queued[i].Data["messageId"] != expected {
			t.Fatal("history rewritten or new generation absent", queued[i])
		}
		_, err := imapadapter.ResolveMessageReference(c, expected)
		if (i < 2 && err == nil) || (i == 2 && err != nil) {
			t.Fatal("notification reference fence", i, err)
		}
	}
	if msg.ID != "1" {
		t.Fatal("internal message identity changed")
	}
	if queued[2].Body != "You have a new email." || queued[2].Data["body"] != "" || queued[2].Data["sender"] != "" {
		t.Fatal("PGP notification preview changed", queued[2])
	}
}
