package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// scriptedSMTP accepts any number of loopback sessions and answers each DATA
// with reply; "" hangs up instead, which leaves acceptance uncertain. It
// counts connections, so "nothing was sent" means nothing even dialed.
// ponytail: a multi-session sibling of captureSMTPDelivery.
type scriptedSMTP struct {
	host  string
	port  int
	mu    sync.Mutex
	dials int
	data  []string
}

func newScriptedSMTP(t *testing.T, reply string) *scriptedSMTP {
	t.Helper()
	allowLoopbackSMTP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &scriptedSMTP{host: "127.0.0.1", port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.dials++
			s.mu.Unlock()
			go s.serve(conn, reply)
		}
	}()
	return s
}

func (s *scriptedSMTP) serve(conn net.Conn, reply string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	_, _ = conn.Write([]byte("220 ready\r\n"))
	var data strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case inData && line == ".\r\n":
			s.mu.Lock()
			s.data = append(s.data, data.String())
			s.mu.Unlock()
			if reply == "" {
				return
			}
			inData = false
			_, _ = conn.Write([]byte(reply + "\r\n"))
		case inData:
			data.WriteString(line)
		case strings.HasPrefix(line, "QUIT"):
			_, _ = conn.Write([]byte("221 bye\r\n"))
			return
		case line == "DATA\r\n":
			inData = true
			_, _ = conn.Write([]byte("354 go\r\n"))
		default:
			_, _ = conn.Write([]byte("250 ok\r\n"))
		}
	}
}

func (s *scriptedSMTP) counts() (dials, data int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials, len(s.data)
}

type scheduledFixture struct {
	srv    *Server
	userID string
	fake   *fakeMailClient
	smtp   *scriptedSMTP
}

// newScheduledFixture is an IMAP account whose SMTP is a scriptedSMTP and
// whose mailbox is a fake that records drafts.
func newScheduledFixture(t *testing.T, reply string) *scheduledFixture {
	t.Helper()
	previous := mailmsg.AllowInsecureSMTP
	mailmsg.AllowInsecureSMTP = true // Test-only loopback SMTP transport.
	t.Cleanup(func() { mailmsg.AllowInsecureSMTP = previous })
	srv := newTestServer(t)
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	f := &scheduledFixture{srv: srv, userID: srv.mustBootstrapUserID(t), fake: &fakeMailClient{}, smtp: newScriptedSMTP(t, reply)}
	f.useAccount(t, f.userID)
	return f
}

func (f *scheduledFixture) useAccount(t *testing.T, userID string) {
	t.Helper()
	f.userID = userID
	if err := writeIMAPConfigPayload(f.srv.userIMAPConfigPath(userID), f.srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw", Mailbox: "INBOX", SMTPHost: f.smtp.host, SMTPPort: f.smtp.port, UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.userMu.Lock()
	f.srv.userMail[userID] = &serverMailEntry{client: f.fake, updatedAt: "test"}
	f.srv.userMu.Unlock()
}

// post sends a request as the account's session through the real routes.
func (f *scheduledFixture) post(path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	return rec
}

func scheduledID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Scheduled bool   `json:"scheduled"`
		ID        string `json:"id"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.Scheduled || got.ID == "" {
		t.Fatalf("schedule: %d %s", rec.Code, rec.Body.String())
	}
	return got.ID
}

func (f *scheduledFixture) schedule(t *testing.T, at time.Time, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"to": "bob@example.com", "subject": "later", "body": "hello later", "mode": "plain", "sendAt": at.Format(time.RFC3339)}
	for k, v := range extra {
		body[k] = v
	}
	return scheduledID(t, f.post("/api/mail/send", body))
}

func (f *scheduledFixture) row(t *testing.T, id string) state.ScheduledSend {
	t.Helper()
	row, found, err := f.srv.globalStore.GetScheduledSend(context.Background(), id)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	return row
}

func sendTime() time.Time { return time.Now().Add(2 * time.Minute).Truncate(time.Second) }

func TestScheduledSendIsDatedAndSentAtSendTime(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	id := f.schedule(t, at, nil)
	f.srv.sweepScheduledSends(context.Background(), clockAt(at.Add(-time.Second)))
	if dials, _ := f.smtp.counts(); dials != 0 || f.row(t, id).Status != state.ScheduledWaiting {
		t.Fatal("sent before its time")
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(at))
	if row := f.row(t, id); row.Status != "sent" || row.Sealed != nil {
		t.Fatalf("row after send: %+v", row)
	}
	f.smtp.mu.Lock()
	defer f.smtp.mu.Unlock()
	if len(f.smtp.data) != 1 || !strings.Contains(f.smtp.data[0], "Date: "+at.UTC().Format(time.RFC1123Z)+"\r\n") {
		t.Fatalf("delivered %d messages, want one dated %s:\n%v", len(f.smtp.data), at.UTC().Format(time.RFC1123Z), f.smtp.data)
	}
	if len(f.fake.savedDrafts) != 0 {
		t.Fatal("a sent message was also returned to Drafts")
	}
}

func TestParseSendAtBounds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for raw, ok := range map[string]bool{
		"":                          true,
		"2026-01-01T00:01:00Z":      true,
		"2026-01-01T00:00:59Z":      false,
		"2027-01-02T00:00:00Z":      true,
		"2027-01-02T00:00:01Z":      false,
		"2026-01-01 05:00":          false,
		"2026-01-01T00:30:00+01:00": false,
	} {
		if _, err := parseSendAt(raw, now); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", raw, err, ok)
		}
	}
}

func TestScheduledSendRefusals(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime().Format(time.RFC3339)
	const ics = "BEGIN:VCALENDAR\nVERSION:2.0\nMETHOD:REPLY\nBEGIN:VEVENT\nUID:e1@x.test\nATTENDEE;PARTSTAT=ACCEPTED:mailto:alice@example.com\nEND:VEVENT\nEND:VCALENDAR\n"
	for name, body := range map[string]map[string]any{
		"too soon":       {"to": "bob@example.com", "body": "b", "sendAt": time.Now().Add(30 * time.Second).Format(time.RFC3339)},
		"too late":       {"to": "bob@example.com", "body": "b", "sendAt": time.Now().Add(367 * 24 * time.Hour).Format(time.RFC3339)},
		"calendar reply": {"to": "organizer@x.test", "body": "b", "sendAt": at, "calendarReply": map[string]string{"ics": ics}},
		"bcc only":       {"bcc": "bob@example.com", "body": "b", "sendAt": at},
	} {
		if rec := f.post("/api/mail/send", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	at2 := sendTime()
	delivery := scheduledPGPDelivery("alice@example.com", at2)
	for name, body := range map[string]clientEncryptedSendRequest{
		"pgp bcc only":     {Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: delivery}}, BCC: []string{"bob@example.com"}, DraftCopy: delivery, SendAt: at2.Format(time.RFC3339)},
		"pgp no draft":     {Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: delivery}}, To: []string{"bob@example.com"}, SendAt: at2.Format(time.RFC3339)},
		"pgp date not set": {Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: scheduledPGPDelivery("alice@example.com", time.Now())}}, To: []string{"bob@example.com"}, DraftCopy: delivery, SendAt: at2.Format(time.RFC3339)},
	} {
		if rec := f.post("/api/mail/send-pgp", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rows, _ := f.srv.globalStore.ListScheduledSends(context.Background(), f.userID, f.userID)
	if dials, _ := f.smtp.counts(); dials != 0 || len(rows) != 0 {
		t.Fatalf("a refused request scheduled or sent mail: %d rows, %d dials", len(rows), dials)
	}

	// Keyless recipients would need a pickup link, which is never scheduled,
	// even when the sender opted in to it.
	srv, _ := newPickupGateServer(t)
	body, _ := json.Marshal(map[string]any{"to": "bob@example.com, carol@example.com", "body": "hello", "encrypt": true, "allowPickupFallback": true, "sendAt": at})
	req := httptest.NewRequest(http.MethodPost, "/api/mail/send", bytes.NewReader(body))
	authRequest(srv, req)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleMailSend)(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"pickupFallbackAvailable":false`) {
		t.Fatalf("keyless scheduled send: %d %s", rec.Code, rec.Body.String())
	}
}

func scheduledPGPDelivery(from string, at time.Time) string {
	return "From: " + from + "\r\nTo: bob@example.com\r\nSubject: ...\r\nDate: " + at.UTC().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--b\r\nContent-Type: application/octet-stream\r\n\r\n" +
		"-----BEGIN PGP MESSAGE-----\r\nAAAA\r\n-----END PGP MESSAGE-----\r\n--b--\r\n"
}

// Every change to the authority the job was frozen under ends it in Drafts
// with nothing dialed.
func TestScheduledSendReturnsToDraftsWhenSecurityChanges(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, reason string
		before       func(t *testing.T, f *scheduledFixture) map[string]any
		change       func(t *testing.T, f *scheduledFixture)
	}{
		{name: "password changed", reason: reasonAccount, change: func(t *testing.T, f *scheduledFixture) {
			if _, err := f.srv.users.SetPassword(ctx, f.userID, "a-new-password-for-test", false, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "totp reset", reason: reasonAccount, before: func(t *testing.T, f *scheduledFixture) map[string]any {
			if _, err := f.srv.users.SetPendingTOTPSecret(f.userID, "sealed-secret"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.srv.users.EnableTOTP(f.userID, "sealed-secret", "now", nil); err != nil {
				t.Fatal(err)
			}
			return nil
		}, change: func(t *testing.T, f *scheduledFixture) {
			if _, err := f.srv.users.DisableTOTP(f.userID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pgp revision", reason: reasonKeys, change: func(t *testing.T, f *scheduledFixture) {
			if _, err := f.srv.users.SetPGPIdentityClientProtected(f.userID, "FPR123", "KID123", "-----BEGIN PGP PUBLIC KEY BLOCK-----\npub\n-----END PGP PUBLIC KEY BLOCK-----", `{"v":2}`, "generated", "now", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "alias removed", reason: reasonFrom, before: func(t *testing.T, f *scheduledFixture) map[string]any {
			aliases, err := f.srv.userSendAsStore(f.userID)
			if err != nil {
				t.Fatal(err)
			}
			alias, err := aliases.Create(f.userID, "alias@example.com", "Alias")
			if err == nil {
				err = aliases.MarkVerified(alias.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			return map[string]any{"from": alias.Email}
		}, change: func(t *testing.T, f *scheduledFixture) {
			aliases, _ := f.srv.userSendAsStore(f.userID)
			all, _ := aliases.List()
			if err := aliases.Delete(all[0].ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "imap account changed", reason: reasonMail, change: func(t *testing.T, f *scheduledFixture) {
			if err := writeIMAPConfigPayload(f.srv.userIMAPConfigPath(f.userID), f.srv.imapConfigKeyPath, imapConfigPayload{
				Host: "imap.example.com", Port: 993, Username: "mallory@example.com", Password: "pw", Mailbox: "INBOX", SMTPHost: f.smtp.host, SMTPPort: f.smtp.port, UpdatedAt: "test",
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScheduledFixture(t, "250 queued")
			var extra map[string]any
			if tc.before != nil {
				extra = tc.before(t, f)
			}
			at := sendTime()
			id := f.schedule(t, at, extra)
			tc.change(t, f)
			f.srv.sweepScheduledSends(ctx, clockAt(at))
			assertReturned(t, f, id, tc.reason)
		})
	}
}

func assertReturned(t *testing.T, f *scheduledFixture, id, reason string) {
	t.Helper()
	row := f.row(t, id)
	if row.Status != "returned" || row.Reason != reason || row.Sealed != nil {
		t.Fatalf("row %+v, want returned with %q", row, reason)
	}
	if len(f.fake.savedDrafts) != 1 || len(f.fake.savedDrafts[0].To) != 1 || f.fake.savedDrafts[0].To[0] != "bob@example.com" {
		t.Fatalf("drafts saved: %+v", f.fake.savedDrafts)
	}
	if dials, _ := f.smtp.counts(); dials != 0 {
		t.Fatalf("%d SMTP connections for mail that must not be sent", dials)
	}
}

func TestScheduledSendReturnsWhenDeviceRevoked(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	store, err := f.srv.userStore(f.userID)
	if err != nil {
		t.Fatal(err)
	}
	device := state.NativeDevice{DeviceID: "phone-1", Platform: "android", PushToken: "t", UserID: f.userID, SecretHash: "hash-1"}
	if err := store.UpsertNativeDevice(device); err != nil {
		t.Fatal(err)
	}
	u, _ := f.srv.users.ClearMustChangePassword(f.userID)
	at := sendTime()
	body, _ := json.Marshal(map[string]any{"to": "bob@example.com", "body": "from my phone", "sendAt": at.Format(time.RFC3339)})
	req := httptest.NewRequest(http.MethodPost, "/api/mail/send", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: f.userID, DeviceID: device.DeviceID, DeviceWitness: state.NativeDeviceWitness(device), NativeSendEpoch: u.NativeSendEpoch}))
	rec := httptest.NewRecorder()
	f.srv.handleMailSend(rec, req)
	id := scheduledID(t, rec)
	if _, err := store.RemoveNativeDevice(device.DeviceID); err != nil {
		t.Fatal(err)
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(at))
	assertReturned(t, f, id, reasonDevice)
}

func TestScheduledSendReturnsWhenMaterialGenerationChanges(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	u := convertedUser(t, f.srv)
	f.useAccount(t, u.ID)
	at := sendTime()
	delivery := scheduledPGPDelivery("alice@example.com", at)
	generation := uint64(1)
	id := scheduledID(t, f.post("/api/mail/send-pgp", clientEncryptedSendRequest{
		Deliveries: []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: delivery}},
		To:         []string{"bob@example.com"}, SentCopy: delivery, SentCopyEncrypted: true, DraftCopy: delivery,
		MaterialGeneration: &generation, SendAt: at.Format(time.RFC3339),
	}))
	// A rotation to the next keyring generation.
	rotated, err := pgpmailtest.GenerateIdentity("Ring", "ring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(rotated.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = f.srv.users.Get(u.ID)
	if _, err := f.srv.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{
		ExpectedRevision: u.PGPRevision, MaterialGeneration: 2, PrimaryFingerprints: append(u.PGPKeyring.PrimaryFingerprints, info.Fingerprint), KeyFingerprints: append(u.PGPKeyring.KeyFingerprints, info.KeyFingerprints...),
		PublicKey: info.ArmoredPublicKey, PasswordEnvelope: `{"v":2,"password":"ring2"}`, RecoveryEnvelope: `{"v":2,"recovery":"ring2"}`, Source: "generated",
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(at))
	assertReturned(t, f, id, reasonKeys)
	if string(f.fake.savedDrafts[0].Raw) != strings.TrimSpace(delivery) {
		t.Fatal("returned draft is not the browser's encrypted draftCopy")
	}
}

func TestScheduledSendRetriesDefiniteFailureThenReturns(t *testing.T) {
	f := newScheduledFixture(t, "451 4.3.0 try later")
	at := sendTime()
	id := f.schedule(t, at, nil)
	ctx := context.Background()
	f.srv.sweepScheduledSends(ctx, clockAt(at))
	if row := f.row(t, id); row.Status != state.ScheduledWaiting {
		t.Fatalf("definite failure not retried: %+v", row)
	}
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(time.Minute)))
	if _, data := f.smtp.counts(); data != 1 {
		t.Fatal("retried before its delay", data)
	}
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryDelay)))
	if _, data := f.smtp.counts(); data != 2 {
		t.Fatal("not retried after its delay", data)
	}
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryWindow+time.Minute)))
	row := f.row(t, id)
	if row.Status != "returned" || row.Reason != reasonLate || len(f.fake.savedDrafts) != 1 {
		t.Fatalf("past the retry window: %+v, %d drafts", row, len(f.fake.savedDrafts))
	}
	if _, data := f.smtp.counts(); data != 2 {
		t.Fatal("sent after the retry window", data)
	}
}

func TestScheduledSendUncertainIsNeverRetried(t *testing.T) {
	f := newScheduledFixture(t, "")
	at := sendTime()
	id := f.schedule(t, at, nil)
	ctx := context.Background()
	f.srv.sweepScheduledSends(ctx, clockAt(at))
	if row := f.row(t, id); row.Status != "uncertain" || row.Sealed != nil {
		t.Fatalf("lost acknowledgement: %+v", row)
	}
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryDelay)))
	f.srv.sweepScheduledSends(ctx, clockAt(at.Add(scheduledRetryWindow)))
	if dials, _ := f.smtp.counts(); dials != 1 || len(f.fake.savedDrafts) != 0 {
		t.Fatalf("uncertain send retried or returned: %d dials, %d drafts", dials, len(f.fake.savedDrafts))
	}
}

func TestScheduledSweeperStartMarksInterruptedSendsUncertain(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	id := f.schedule(t, at, nil)
	if _, ok, err := f.srv.globalStore.ClaimScheduledSend(context.Background(), at.Unix()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.srv.StartScheduledSendSweeper(ctx)
	if row := f.row(t, id); row.Status != "uncertain" || row.Reason != state.ScheduledInterrupted {
		t.Fatalf("interrupted send: %+v", row)
	}
	if dials, _ := f.smtp.counts(); dials != 0 {
		t.Fatal("interrupted send was resent")
	}
}

// Exactly one of cancel and claim wins each race; a lost cancel answers 409.
func TestScheduledCancelRacesClaim(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	for range 20 {
		id := f.schedule(t, at, nil)
		var wg sync.WaitGroup
		var rec *httptest.ResponseRecorder
		var claimed bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec = f.post("/api/scheduled/"+id+"/cancel", nil)
		}()
		go func() {
			defer wg.Done()
			row, ok, err := f.srv.globalStore.ClaimScheduledSend(context.Background(), at.Unix())
			claimed = ok && err == nil && row.ID == id
		}()
		wg.Wait()
		canceled := rec.Code == http.StatusOK || rec.Code == http.StatusAccepted
		if canceled == claimed || !canceled && rec.Code != http.StatusConflict {
			t.Fatalf("cancel %d %s, claimed %v", rec.Code, rec.Body.String(), claimed)
		}
		if claimed {
			_, _ = f.srv.globalStore.MoveScheduledSend(context.Background(), id, state.ScheduledSending, "sent", "", 0)
		}
	}
}

func TestScheduledListAndCancel(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	id := f.schedule(t, at, map[string]any{"subject": "quarterly report"})
	req := httptest.NewRequest(http.MethodGet, "/api/scheduled", nil)
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	var list struct {
		Scheduled []map[string]any `json:"scheduled"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Scheduled) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	got := list.Scheduled[0]
	if got["id"] != id || got["status"] != state.ScheduledWaiting || got["subject"] != "quarterly report" || got["sendAt"] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("listed %v", got)
	}
	if strings.Contains(rec.Body.String(), "hello later") || strings.Contains(rec.Body.String(), "sealed") {
		t.Fatal("listing leaked the frozen message")
	}
	other, err := f.srv.users.Create(context.Background(), "someone-else", "someone-else-password", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	owner := f.userID
	f.userID = other.ID
	if rec := f.post("/api/scheduled/"+id+"/cancel", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user canceled it: %d", rec.Code)
	}
	f.userID = owner
	if rec := f.post("/api/scheduled/"+id+"/cancel", nil); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	assertReturned(t, f, id, reasonCanceled)
	if rec := f.post("/api/scheduled/"+id+"/cancel", nil); rec.Code != http.StatusConflict {
		t.Fatalf("second cancel: %d", rec.Code)
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(at))
	if len(f.fake.savedDrafts) != 1 {
		t.Fatal("canceled draft saved twice")
	}
}

func clockAt(at time.Time) func() time.Time { return func() time.Time { return at } }

type failingDraftClient struct{ *fakeMailClient }

func (failingDraftClient) SaveDraft(context.Context, imapadapter.DraftMessage) error {
	return errors.New("injected: mailbox unreachable")
}

// A draft that can never be saved is retried for a week, then given up, so a
// dead mailbox cannot keep a sweep busy forever.
func TestScheduledReturnGivesUpAfterAWeek(t *testing.T) {
	f := newScheduledFixture(t, "250 OK")
	id := f.schedule(t, sendTime(), nil)
	f.srv.userMu.Lock()
	f.srv.userMail[f.userID] = &serverMailEntry{client: failingDraftClient{f.fake}, updatedAt: "test"}
	f.srv.userMu.Unlock()
	if rec := f.post("/api/scheduled/"+id+"/cancel", map[string]any{}); rec.Code != 202 {
		t.Fatalf("cancel with a failing mailbox: %d %s", rec.Code, rec.Body.String())
	}
	if row := f.row(t, id); row.Status != state.ScheduledReturning || row.ReturningSince == 0 || row.Sealed == nil {
		t.Fatalf("after failed save: %+v; want returning, stamped, job kept", row)
	}
	since := time.Unix(f.row(t, id).ReturningSince, 0)

	f.srv.sweepScheduledSends(context.Background(), clockAt(since.Add(time.Hour)))
	if row := f.row(t, id); row.Status != state.ScheduledReturning {
		t.Fatalf("gave up after an hour: %+v", row)
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(since.Add(scheduledReturnFor+time.Hour)))
	if row := f.row(t, id); row.Status != "failed" || row.Reason != reasonNoDraft || row.Sealed != nil {
		t.Fatalf("after a week: %+v; want failed, %q, job cleared", row, reasonNoDraft)
	}
	if dials, _ := f.smtp.counts(); dials != 0 {
		t.Fatalf("%d SMTP connections for canceled mail", dials)
	}
}

// An idle server never creates or reads the key; only scheduling creates it.
// (The sweeper goroutine is not awaited on shutdown, so a write here raced
// test cleanup.)
func TestScheduledSweepIdleLeavesKeyAlone(t *testing.T) {
	f := newScheduledFixture(t, "250 OK")
	keyPath := filepath.Join(config.SecretDir(), "scheduled-send.key")
	f.srv.sweepScheduledSends(context.Background(), time.Now)
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle sweep touched %s: %v", keyPath, err)
	}
	f.schedule(t, sendTime(), nil)
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("scheduling did not create the key: %v", err)
	}
}

// A sealed job over the row cap is refused with 413 and nothing is stored.
func TestScheduledSendRefusesOversizedRow(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	previous := scheduledMaxRowBytes
	scheduledMaxRowBytes = 64 << 10
	t.Cleanup(func() { scheduledMaxRowBytes = previous })
	f.schedule(t, sendTime(), nil)
	rec := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": strings.Repeat("x", scheduledMaxRowBytes), "sendAt": sendTime().Format(time.RFC3339)})
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "too large to schedule") {
		t.Fatalf("oversized schedule: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := f.srv.globalStore.ListScheduledSends(context.Background(), f.userID, f.userID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("oversized schedule stored a row: %d rows, %v", len(rows), err)
	}
	if dials, _ := f.smtp.counts(); dials != 0 {
		t.Fatal("oversized schedule sent mail")
	}
}

// The deadline is checked against the clock when each row is worked: a row
// reached only after its deadline passed mid-sweep returns late, unsent.
func TestScheduledSweepReadsClockPerRow(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	at := sendTime()
	ids := []string{f.schedule(t, at, nil), f.schedule(t, at, nil)}
	// One user's rows are worked one at a time, so the second is claimed
	// only after the first was delivered; by then its deadline has passed.
	late := at.Add(scheduledRetryWindow + time.Minute)
	f.srv.sweepScheduledSends(context.Background(), func() time.Time {
		if _, data := f.smtp.counts(); data > 0 {
			return late
		}
		return at
	})
	statuses := map[string]string{}
	for _, id := range ids {
		row := f.row(t, id)
		statuses[row.Status] = row.Reason
	}
	if _, sent := statuses["sent"]; !sent || statuses["returned"] != reasonLate || len(statuses) != 2 {
		t.Fatalf("rows ended %v; want one sent and one returned late", statuses)
	}
	if _, data := f.smtp.counts(); data != 1 || len(f.fake.savedDrafts) != 1 {
		t.Fatalf("%d messages delivered, %d drafts; want 1 and 1", data, len(f.fake.savedDrafts))
	}
}
