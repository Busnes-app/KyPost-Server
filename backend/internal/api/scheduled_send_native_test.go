//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// A due native job enters the outbox only then, bound to the relay
// generation current at that moment, and a canceled one lands in native Drafts.
func TestScheduledNativeSendQueuesWithCurrentRelay(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "scheduled-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	relayPath, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	saveRelay := func() mailmsg.DomainRelay {
		if _, err := mailmsg.SaveDomainRelay(context.Background(), relayPath, keyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: 1, Username: "operator-login", Password: "operator-secret"}); err != nil {
			t.Fatal(err)
		}
		relay, _, err := mailmsg.ReadDomainRelay(relayPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		return relay
	}
	scheduledRelay := saveRelay()
	at := sendTime()
	schedule := func() string {
		body, _ := json.Marshal(map[string]any{"to": "visible@outside.test", "subject": "native later", "body": "native body", "sendAt": at.Format(time.RFC3339)})
		r := httptest.NewRequest("POST", "/api/mail/send", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		authRequestAs(s, r, u.ID)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return scheduledID(t, w)
	}
	id, canceled := schedule(), schedule()

	a, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := box.ReadOutbound(context.Background(), key, id); err == nil {
		t.Fatal("scheduled job entered the outbox before it was due")
	}

	r := httptest.NewRequest("POST", "/api/scheduled/"+canceled+"/cancel", nil)
	authRequestAs(s, r, u.ID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if drafts, err := box.List(context.Background(), "Drafts", 0, 10); err != nil || len(drafts) != 1 {
		t.Fatal("canceled native mail not in Drafts", drafts, err)
	}

	current := saveRelay()
	if current.Generation == scheduledRelay.Generation {
		t.Fatal("relay generation did not rotate")
	}
	s.sweepScheduledSends(context.Background(), clockAt(at))
	row, _, _ := s.globalStore.GetScheduledSend(context.Background(), id)
	if row.Status != "queued" {
		t.Fatalf("native scheduled send: %+v", row)
	}
	job, statuses, _, err := box.ReadOutbound(context.Background(), key, id)
	if err != nil || len(statuses) != 1 || statuses[0].State != "queued" || job.RelayGeneration != current.Generation {
		t.Fatalf("outbox job %v %v relay %q, want %q", statuses, err, job.RelayGeneration, current.Generation)
	}
}

// nativeScheduled is a native mailbox user with a relay, its mailbox open.
type nativeScheduled struct {
	s   *Server
	u   users.User
	box *mailbox.Store
	key []byte
}

func newNativeScheduled(t *testing.T) *nativeScheduled {
	t.Helper()
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "scheduled-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(config.SecretDir(), "native-relay.key")
	if _, err := mailmsg.SaveDomainRelay(context.Background(), filepath.Join(s.configDir, "native-relay.json"), keyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: 1, Username: "operator-login", Password: "operator-secret"}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeScheduled{s: s, u: u, box: box, key: key}
}

// schedule posts body to path as the user's session, or as a device when
// auth is set, and returns the scheduled row's ID.
func (f *nativeScheduled) schedule(t *testing.T, path string, body any, auth func(*http.Request)) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	if auth == nil {
		authRequestAs(f.s, r, f.u.ID)
	} else {
		auth(r)
	}
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return scheduledID(t, w)
}

// Every change to the authority a native job was frozen under ends it in the
// native Drafts with nothing queued for the outbox worker to send.
func TestScheduledNativeSendReturnsToDraftsWhenSecurityChanges(t *testing.T) {
	ctx := context.Background()
	plain := func(at time.Time, from string) map[string]any {
		return map[string]any{"to": "visible@outside.test", "subject": "native later", "body": "native body", "from": from, "sendAt": at.Format(time.RFC3339)}
	}
	var device struct{ id, secret string }
	for _, tc := range []struct {
		name, reason string
		schedule     func(t *testing.T, f *nativeScheduled, at time.Time) string
		change       func(t *testing.T, f *nativeScheduled)
	}{
		{name: "password changed", reason: reasonAccount, change: func(t *testing.T, f *nativeScheduled) {
			before, _ := f.s.users.Get(f.u.ID)
			u, err := f.s.users.SetPassword(ctx, f.u.ID, "a-new-password-for-test", false, nil)
			if err != nil || u.NativeSendEpoch == before.NativeSendEpoch {
				t.Fatal("send epoch not advanced", err)
			}
		}},
		{name: "pgp revision", reason: reasonKeys, change: func(t *testing.T, f *nativeScheduled) {
			if _, err := f.s.users.SetPGPIdentityClientProtected(f.u.ID, "FPR123", "KID123", "-----BEGIN PGP PUBLIC KEY BLOCK-----\npub\n-----END PGP PUBLIC KEY BLOCK-----", `{"v":2}`, "generated", "now", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "keyring generation", reason: reasonKeys, schedule: func(t *testing.T, f *nativeScheduled, at time.Time) string {
			convertUser(t, f.s, f.u)
			delivery := scheduledPGPDelivery("one@example.test", at)
			generation := uint64(1)
			return f.schedule(t, "/api/mail/send-pgp", clientEncryptedSendRequest{
				Deliveries: []clientEncryptedDelivery{{Recipients: []string{"visible@outside.test"}, Ciphertext: delivery}},
				To:         []string{"visible@outside.test"}, SentCopy: delivery, SentCopyEncrypted: true, DraftCopy: delivery,
				MaterialGeneration: &generation, SendAt: at.Format(time.RFC3339),
			}, nil)
		}, change: func(t *testing.T, f *nativeScheduled) {
			rotated, err := pgpmail.GenerateIdentity("Ring", "ring@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			info, err := pgpmail.InspectPublicKey(rotated.ArmoredPublicKey)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := f.s.users.Get(f.u.ID)
			if _, err := f.s.users.CommitPGPKeyring(ctx, u.ID, users.PGPKeyringUpdate{
				ExpectedRevision: u.PGPRevision, MaterialGeneration: 2, PrimaryFingerprints: append(u.PGPKeyring.PrimaryFingerprints, info.Fingerprint), KeyFingerprints: append(u.PGPKeyring.KeyFingerprints, info.KeyFingerprints...),
				PublicKey: info.ArmoredPublicKey, PasswordEnvelope: `{"v":2,"password":"ring2"}`, RecoveryEnvelope: `{"v":2,"recovery":"ring2"}`, Source: "generated",
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "device paired again", reason: reasonDevice, schedule: func(t *testing.T, f *nativeScheduled, at time.Time) string {
			device.id, device.secret = pairNativeDevice(t, f.s, f.u.ID, "phone-1")
			return f.schedule(t, "/api/mail/send", plain(at, ""), func(r *http.Request) { setDeviceHeaders(r, device.id, device.secret) })
		}, change: func(t *testing.T, f *nativeScheduled) {
			pairNativeDevice(t, f.s, f.u.ID, device.id)
		}},
		{name: "device removed", reason: reasonDevice, schedule: func(t *testing.T, f *nativeScheduled, at time.Time) string {
			device.id, device.secret = pairNativeDevice(t, f.s, f.u.ID, "phone-1")
			return f.schedule(t, "/api/mail/send", plain(at, ""), func(r *http.Request) { setDeviceHeaders(r, device.id, device.secret) })
		}, change: func(t *testing.T, f *nativeScheduled) {
			store, err := f.s.userStore(f.u.ID)
			if err == nil {
				_, err = store.RemoveNativeDevice(device.id)
			}
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "from released", reason: reasonSender, schedule: func(t *testing.T, f *nativeScheduled, at time.Time) string {
			if _, err := f.s.ssoLifecycle.AddNativeAlias(ctx, f.s.stateDir, f.u.ID, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
			return f.schedule(t, "/api/mail/send", plain(at, "sales@example.test"), nil)
		}, change: func(t *testing.T, f *nativeScheduled) {
			if _, err := f.s.ssoLifecycle.ReleaseNativeAlias(ctx, f.s.stateDir, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
		}},
		// Released and handed back to the same mailbox: active again, but at a
		// new generation, so the frozen job must not inherit it.
		{name: "from generation changed", reason: reasonSender, schedule: func(t *testing.T, f *nativeScheduled, at time.Time) string {
			if _, err := f.s.ssoLifecycle.AddNativeAlias(ctx, f.s.stateDir, f.u.ID, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
			return f.schedule(t, "/api/mail/send", plain(at, "sales@example.test"), nil)
		}, change: func(t *testing.T, f *nativeScheduled) {
			if _, err := f.s.ssoLifecycle.ReleaseNativeAlias(ctx, f.s.stateDir, "sales@example.test"); err != nil {
				t.Fatal(err)
			}
			x, err := f.s.ssoLifecycle.ReassignNativeAddress(ctx, f.s.stateDir, "sales@example.test", f.u.ID)
			if err != nil || x.State != "active" {
				t.Fatal("alias not active again", x, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeScheduled(t)
			at := sendTime()
			var id string
			if tc.schedule != nil {
				id = tc.schedule(t, f, at)
			} else {
				id = f.schedule(t, "/api/mail/send", plain(at, ""), nil)
			}
			tc.change(t, f)
			f.s.sweepScheduledSends(ctx, clockAt(at))
			if _, _, _, err := f.box.ReadOutbound(ctx, f.key, id); err == nil {
				t.Fatal("the scheduled send was queued for the outbox")
			}
			if pending, err := f.box.PendingOutbound(ctx, 10); err != nil || len(pending) != 0 {
				t.Fatal("outbox holds work after a security change", pending, err)
			}
			row, _, _ := f.s.globalStore.GetScheduledSend(ctx, id)
			if row.Status != "returned" || row.Reason != tc.reason || row.Sealed != nil {
				t.Fatalf("row %s %q (sealed %t), want returned with %q", row.Status, row.Reason, row.Sealed != nil, tc.reason)
			}
			if drafts, err := f.box.List(ctx, "Drafts", 0, 10); err != nil || len(drafts) != 1 {
				t.Fatal("returned native mail not in Drafts", drafts, err)
			}
		})
	}
}

// qualifyScheduledNativeDelivery runs in TestNativeOutboundAPIActualTLSAndPGP's
// child process: a scheduled native send enters the outbox only when due, then
// the outbox worker's pass (Pending, Recover) delivers it over TLS and files
// Sent, both dated sendAt.
func qualifyScheduledNativeDelivery(t *testing.T, s *Server, userID, root string) {
	t.Helper()
	ctx := context.Background()
	at := sendTime()
	raw, _ := json.Marshal(map[string]any{"to": "visible@outside.test", "bcc": "hidden@outside.test", "subject": "native later", "body": "native body", "sendAt": at.Format(time.RFC3339)})
	r := httptest.NewRequest("POST", "/api/mail/send", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	authRequestAs(s, r, userID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	id := scheduledID(t, w)
	sender := s.nativeOutbound()
	if pending, err := sender.Pending(ctx, userID, 50); err != nil || len(pending) != 0 {
		t.Fatal("outbox work before the send time", pending, err)
	}
	s.sweepScheduledSends(ctx, clockAt(at))
	if row, _, _ := s.globalStore.GetScheduledSend(ctx, id); row.Status != "queued" {
		t.Fatalf("due native send %s %q, want queued", row.Status, row.Reason)
	}
	pending, err := sender.Pending(ctx, userID, 50)
	if err != nil || len(pending) != 1 || pending[0] != id {
		t.Fatal("due send not pending in the outbox", pending, err)
	}
	if err := sender.Recover(ctx, userID, id); err != nil {
		t.Fatal(err)
	}
	statuses, saved, err := sender.Status(ctx, userID, id)
	if err != nil || len(statuses) != 1 || statuses[0].State != "accepted" || !saved {
		t.Fatal("scheduled native send not delivered and filed", statuses, saved, err)
	}
	a, _, err := s.nativeMailAssignment(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(userID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	messages, err := box.List(ctx, "Sent", 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatal("Sent missing or duplicated", messages, err)
	}
	sent, err := box.Raw(ctx, "Sent", messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sent, []byte("hidden@outside.test")) {
		t.Fatal("Sent lost BCC")
	}
	wire, err := os.ReadFile(filepath.Join(root, "received.eml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"delivered": wire, "Sent": sent} {
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if date, err := msg.Header.Date(); err != nil || !date.Equal(at) {
			t.Fatalf("%s Date %q, want the send time %s", name, msg.Header.Get("Date"), at)
		}
	}
}
