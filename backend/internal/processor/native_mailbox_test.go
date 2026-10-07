//go:build linux

package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// The poller classifies each active mailbox into its own state, notifies
// through the primary's devices with the mailbox ID, and skips a disabled one.
func TestNativePollerPollsEachMailbox(t *testing.T) {
	ctx := context.Background()
	p, _ := newTickTestPoller(t, nil)
	p.EnableNativeMail()
	var logs bytes.Buffer
	logger, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	p.log = logger
	const issuer = "https://identity.example.test"
	if err := sso.NewStore(p.configDir).Save(sso.SSOSettings{Enabled: true, IssuerURL: issuer, ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	life := sso.NewLifecycleStore(p.configDir)
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"runtime","externalId":"runtime","userName":"runtime","active":true,"emails":[{"value":"runtime@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "runtime-create", Type: "user.created", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	domains := sso.NewNativeDomainStore(p.configDir)
	if _, err := domains.Configure(ctx, "example.test", issuer); err != nil {
		t.Fatal(err)
	}
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := domains.Read()
		return []string{d.RecordValue()}, err
	})
	u, err := life.AllocateNativeAccount(ctx, p.stateDir, issuer, "runtime", domains, p.users, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = false
	settings.Notifications.Mode = "all"
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	m, err := life.CreateNativeMailbox(ctx, p.stateDir, u.ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	primary := tickStore(t, p, u)
	if err := primary.UpsertNativeDevice(state.NativeDevice{DeviceID: "phone", Platform: "android", PushToken: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := primary.SetNativeDeliveryMode(state.DeliveryModePull); err != nil {
		t.Fatal(err)
	}
	appendTo := func(mailboxID, subject string) int64 {
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
		id, err := store.Append(ctx, "INBOX", bytes.NewBufferString("From: sender@outside.test\r\nTo: "+a.Address+"\r\nSubject: "+subject+"\r\n\r\nbody"), false)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	primaryID := appendTo("", "primary")
	extraID := appendTo(m.ID, "extra")
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	p.tick()
	a, _, err := p.nativeMailboxAssignment(ctx, u.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := state.OpenNative(a.Dir(p.stateDir), a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	if d := primary.Decisions(10); len(d) != 1 || d[0].MessageID != strconv.FormatInt(primaryID, 10) {
		t.Fatalf("primary decisions %+v", d)
	}
	if d := extra.Decisions(10); len(d) != 1 || d[0].MessageID != strconv.FormatInt(extraID, 10) || d[0].Subject != "extra" {
		t.Fatalf("extra decisions %+v", d)
	}
	queued, _, err := primary.PullNotificationsAfterStrict("phone", 0)
	if err != nil || len(queued) != 2 {
		t.Fatal("notifications", queued, err)
	}
	mailboxes := map[string]bool{}
	for _, n := range queued {
		mailboxes[n.Data["mailbox"]] = true
	}
	if !mailboxes[u.ID] || !mailboxes[m.ID] {
		t.Fatal("notifications do not carry their mailbox ID", queued)
	}
	if devices, err := extra.ListNativeDevicesStrict(); err != nil || len(devices) != 0 {
		t.Fatal("extra mailbox state gained devices", devices, err)
	}
	if pulled, _, err := extra.PullNotificationsAfterStrict("phone", 0); err != nil || len(pulled) != 0 {
		t.Fatal("extra mailbox state gained notifications", pulled, err)
	}
	// Backstop: with incoming encryption on, the extra mailbox is not polled.
	appendTo(m.ID, "while-encrypting")
	setEncrypt := func(on bool) {
		t.Helper()
		if err := config.UpdateUserSettings(p.userSettingsPath(u.ID), func(s *config.UserSettings) error { s.EncryptIncoming = on; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setEncrypt(true)
	logs.Reset()
	p.tick()
	if d := extra.Decisions(10); len(d) != 1 || !bytes.Contains(logs.Bytes(), []byte("extra mailbox not polled")) {
		t.Fatalf("extra mailbox polled beside incoming encryption %+v", d)
	}
	setEncrypt(false)
	// A creation interrupted before publication (no source) is not polled.
	ledgerPath := filepath.Join(p.configDir, "native-provisioning.json")
	raw, err = os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatal(err)
	}
	record := ledger["mailboxes"].(map[string]any)[m.ID].(map[string]any)
	source := record["source"]
	delete(record, "source")
	write := func() {
		t.Helper()
		data, err := json.Marshal(ledger)
		if err == nil {
			err = os.WriteFile(ledgerPath, data, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	write()
	logs.Reset()
	p.tick()
	if d := extra.Decisions(10); len(d) != 1 || bytes.Contains(logs.Bytes(), []byte("mailbox admission refused")) {
		t.Fatalf("unprepared mailbox polled %+v %s", d, logs.Bytes())
	}
	record["source"] = source
	write()
	// A disabled mailbox is not polled; its mail waits.
	appendTo(m.ID, "while-disabled")
	if _, err := life.SetNativeMailboxState(ctx, p.stateDir, m.ID, false); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	p.tick()
	if d := extra.Decisions(10); len(d) != 1 || bytes.Contains(logs.Bytes(), []byte("mailbox admission refused")) {
		t.Fatalf("disabled mailbox polled %+v", d)
	}
}
