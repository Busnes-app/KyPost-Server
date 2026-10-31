package api

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var kycalSecret = []byte(strings.Repeat("k", kycalendar.MinSecretBytes))

const (
	kycalIssuer  = "https://id.example.com"
	inviteICS    = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:e1@example.com\r\nSUMMARY:Planning\r\nORGANIZER;CN=Alice:mailto:alice@example.com\r\nATTENDEE:mailto:bob@x.test\r\nATTENDEE:mailto:carol@x.test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	replyICS     = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:r1@x.test\r\nORGANIZER:mailto:org@x.test\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	smtpWaitTime = 3 * time.Second
)

// smtpSessions accepts loopback SMTP sessions and hands back each DATA.
func smtpSessions(t *testing.T) (string, int, <-chan string) {
	t.Helper()
	allowLoopbackSMTP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
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
						inData = false
						got <- data.String()
						_, _ = conn.Write([]byte("250 queued\r\n"))
					case inData:
						data.WriteString(line)
					case line == "DATA\r\n":
						inData = true
						_, _ = conn.Write([]byte("354 go\r\n"))
					case strings.HasPrefix(line, "QUIT"):
						_, _ = conn.Write([]byte("221 bye\r\n"))
						return
					default:
						_, _ = conn.Write([]byte("250 ok\r\n"))
					}
				}
			}()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, got
}

type kycalFixture struct {
	srv       *Server
	userID    string
	delivered <-chan string
	fake      *fakeMailClient
}

// newKyCalFixture is a paired server whose user (alice@example.com, SSO
// subject sub-1, verified alias team@example.com) turned KyCalendar on.
func newKyCalFixture(t *testing.T) *kycalFixture {
	t.Helper()
	previous := mailmsg.AllowInsecureSMTP
	mailmsg.AllowInsecureSMTP = true // Test-only loopback SMTP transport.
	t.Cleanup(func() { mailmsg.AllowInsecureSMTP = previous })
	srv := newTestServer(t)
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	userID := srv.mustBootstrapUserID(t)
	host, port, delivered := smtpSessions(t)
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw", Mailbox: "INBOX",
		SMTPHost: host, SMTPPort: port, UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.users.LinkSSOIssuer(kycalIssuer, userID, "sub-1", "alice", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := store.Create(userID, "team@example.com", "Team")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkVerified(alias.ID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMailClient{}
	srv.userMu.Lock()
	srv.userMail[userID] = &serverMailEntry{client: fake, updatedAt: "test"}
	srv.userMu.Unlock()
	base, _ := url.Parse("https://cal.example.com")
	srv.SetKyCalendar(kycalendar.New(base, kycalSecret, http.DefaultTransport))
	f := &kycalFixture{srv: srv, userID: userID, delivered: delivered, fake: fake}
	f.setEnabled(t, true)
	return f
}

func (f *kycalFixture) setEnabled(t *testing.T, on bool) {
	t.Helper()
	if err := config.UpdateUserSettings(f.srv.userSettingsPath(f.userID), func(s *config.UserSettings) error {
		s.KyCalendar = &on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func signedSend(t *testing.T, srv *Server, secret []byte, eventType, eventID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	h, err := syncauth.Sign(secret, time.Now(), eventType, eventID, raw)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/itip/send", bytes.NewReader(raw))
	h.Apply(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func sendBody(mutate func(map[string]any)) map[string]any {
	b := map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "from": "alice@example.com",
		"recipients": []string{"bob@x.test", "carol@x.test"}, "method": "REQUEST", "ics": inviteICS}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func answerCode(rec *httptest.ResponseRecorder) string {
	var v struct {
		Code, Outcome string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v.Code + v.Outcome
}

func noDelivery(t *testing.T, f *kycalFixture, name string) {
	t.Helper()
	select {
	case raw := <-f.delivered:
		t.Fatalf("%s: a refused send was delivered:\n%s", name, raw)
	case <-time.After(50 * time.Millisecond):
	}
}

// Every refusal answers its contract code and sends nothing.
func TestITIPSendRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(*testing.T, *kycalFixture)
		secret    []byte
		eventType string
		body      any
		status    int
		code      string
	}{
		"wrong secret":     {secret: []byte(strings.Repeat("x", 32)), body: sendBody(nil), status: 401, code: "bad_signature"},
		"wrong event type": {eventType: kycalendar.EventInbound, body: sendBody(nil), status: 401, code: "bad_signature"},
		"malformed json":   {body: []byte("{"), status: 400, code: "bad_body"},
		"missing from":     {body: sendBody(func(b map[string]any) { delete(b, "from") }), status: 400, code: "bad_body"},
		"bad method":       {body: sendBody(func(b map[string]any) { b["method"] = "PUBLISH" }), status: 400, code: "bad_body"},
		"named recipient":  {body: sendBody(func(b map[string]any) { b["recipients"] = []string{"Bob <bob@x.test>"} }), status: 400, code: "bad_body"},
		"unknown subject":  {body: sendBody(func(b map[string]any) { b["sub"] = "sub-2" }), status: 404, code: "no_user"},
		"other issuer":     {body: sendBody(func(b map[string]any) { b["issuer"] = "https://evil.example" }), status: 404, code: "no_user"},
		"sending off":      {setup: func(t *testing.T, f *kycalFixture) { f.setEnabled(t, false) }, body: sendBody(nil), status: 409, code: "sending_off"},
		"foreign from":     {body: sendBody(func(b map[string]any) { b["from"] = "mallory@example.com" }), status: 409, code: "bad_from"},
		"method mismatch":  {body: sendBody(func(b map[string]any) { b["method"] = "CANCEL" }), status: 422, code: "bad_ics"},
		"huge parameter": {body: sendBody(func(b map[string]any) {
			b["ics"] = strings.Replace(inviteICS, "CN=Alice", "CN="+strings.Repeat("a", 2000), 1)
		}), status: 422, code: "bad_ics"},
		"not an attendee":    {body: sendBody(func(b map[string]any) { b["recipients"] = []string{"bob@x.test", "dave@x.test"} }), status: 409, code: "bad_recipient"},
		"alias not organiz.": {body: sendBody(func(b map[string]any) { b["from"] = "team@example.com" }), status: 409, code: "bad_recipient"},
		"reply to attendee": {body: sendBody(func(b map[string]any) {
			b["method"], b["ics"], b["recipients"] = "REPLY", replyICS, []string{"bob@x.test"}
		}), status: 409, code: "bad_recipient"},
		"reply without invitation": {body: sendBody(func(b map[string]any) {
			b["method"], b["ics"], b["recipients"] = "REPLY", replyICS, []string{"org@x.test"}
		}), status: 409, code: "no_invitation"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKyCalFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			secret := kycalSecret
			if tc.secret != nil {
				secret = tc.secret
			}
			eventType := kycalendar.EventSend
			if tc.eventType != "" {
				eventType = tc.eventType
			}
			rec := signedSend(t, f.srv, secret, eventType, "ev-"+name, tc.body)
			if rec.Code != tc.status || answerCode(rec) != tc.code {
				t.Fatalf("status %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			noDelivery(t, f, name)
		})
	}
}

func TestITIPSendOffWhenUnpaired(t *testing.T) {
	f := newKyCalFixture(t)
	f.srv.SetKyCalendar(nil)
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-1", sendBody(nil)); rec.Code != 404 || answerCode(rec) != "kycalendar_off" {
		t.Fatalf("unpaired: %d %s", rec.Code, rec.Body.String())
	}
	noDelivery(t, f, "unpaired")
}

// A REQUEST reaches the user's SMTP server as a fixed template carrying the
// ics part; caller fields outside the contract reach no header; a replayed
// event ID is answered duplicate without sending again.
func TestITIPSendDeliversRequest(t *testing.T) {
	f := newKyCalFixture(t)
	body := sendBody(func(b map[string]any) {
		b["subject"] = "evil-subject"
		b["headers"] = map[string]string{"Bcc": "evil@x.test"}
		b["recipients"] = []string{"Bob@X.test", "carol@x.test", "bob@x.test"}
	})
	rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-ok", body)
	if rec.Code != http.StatusAccepted || answerCode(rec) != "sent" {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var raw string
	select {
	case raw = <-f.delivered:
	case <-time.After(smtpWaitTime):
		t.Fatal("nothing delivered")
	}
	if strings.Contains(raw, "evil") {
		t.Fatalf("caller text reached the message:\n%s", raw)
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name := range msg.Header {
		switch name {
		case "From", "To", "Subject", "Date", "Message-Id", "Mime-Version", "Content-Type":
		default:
			t.Errorf("unexpected header %s", name)
		}
	}
	if msg.Header.Get("From") != "alice@example.com" || msg.Header.Get("To") != "bob@x.test, carol@x.test" || msg.Header.Get("Subject") != "Planning" {
		t.Fatalf("headers: %v", msg.Header)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	parts := multipart.NewReader(msg.Body, params["boundary"])
	var cal []byte
	for {
		p, err := parts.NextPart()
		if err != nil {
			break
		}
		if ct := p.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/calendar") {
			if ct != "text/calendar; method=REQUEST; charset=UTF-8" {
				t.Fatalf("calendar content type %q", ct)
			}
			enc, _ := io.ReadAll(p)
			cal, _ = base64.StdEncoding.DecodeString(strings.ReplaceAll(string(enc), "\r\n", ""))
		}
	}
	if string(cal) != inviteICS {
		t.Fatalf("calendar part = %q", cal)
	}

	again := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-ok", body)
	if again.Code != http.StatusOK || answerCode(again) != "duplicate" {
		t.Fatalf("replay: %d %s", again.Code, again.Body.String())
	}
	noDelivery(t, f, "replay")
}

// A refusal forgets the event ID, so KyCalendar's retry of it runs again
// once the user has fixed the cause.
func TestITIPSendRetryAfterRefusal(t *testing.T) {
	f := newKyCalFixture(t)
	f.setEnabled(t, false)
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-retry", sendBody(nil)); answerCode(rec) != "sending_off" {
		t.Fatalf("first attempt: %s", rec.Body.String())
	}
	f.setEnabled(t, true)
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-retry", sendBody(nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	<-f.delivered
}

// A REPLY goes out only after an aligned invitation from its organizer was
// recorded, and only to that organizer.
func TestITIPSendReplyNeedsProvenance(t *testing.T) {
	f := newKyCalFixture(t)
	reply := sendBody(func(b map[string]any) {
		b["method"], b["ics"], b["recipients"] = "REPLY", replyICS, []string{"org@x.test"}
	})
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-r1", reply); answerCode(rec) != "no_invitation" {
		t.Fatalf("before provenance: %d %s", rec.Code, rec.Body.String())
	}
	invitations := kycalendar.OpenInvitations(f.srv.userStateDir(f.userID))
	if err := invitations.Record("r1@x.test", "someone-else@x.test"); err != nil {
		t.Fatal(err)
	}
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-r2", reply); answerCode(rec) != "no_invitation" {
		t.Fatalf("other organizer's record: %d %s", rec.Code, rec.Body.String())
	}
	if err := invitations.Record("r1@x.test", "Org@X.test"); err != nil {
		t.Fatal(err)
	}
	rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-r3", reply)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("after provenance: %d %s", rec.Code, rec.Body.String())
	}
	if raw := <-f.delivered; !strings.Contains(raw, "Content-Type: text/calendar; method=REPLY; charset=UTF-8") || !strings.Contains(raw, "Subject: Reply") {
		t.Fatalf("reply message:\n%s", raw)
	}
}

func TestITIPSendChargesTheQuota(t *testing.T) {
	f := newKyCalFixture(t)
	if _, err := f.srv.users.SetSendQuota(f.userID, &config.SendQuota{MessagesPerHour: 1, RecipientsPerDay: 100}); err != nil {
		t.Fatal(err)
	}
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-q1", sendBody(nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	<-f.delivered
	rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-q2", sendBody(nil))
	if rec.Code != http.StatusTooManyRequests || answerCode(rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over quota: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	noDelivery(t, f, "over quota")
}

// KyCalendar answers 503 when the user's SMTP server refuses, and the retry
// of the same event ID is processed again.
func TestITIPSendSMTPFailureIsRetryable(t *testing.T) {
	f := newKyCalFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := writeIMAPConfigPayload(f.srv.userIMAPConfigPath(f.userID), f.srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw", Mailbox: "INBOX",
		SMTPHost: "127.0.0.1", SMTPPort: port, UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-smtp", sendBody(nil)); rec.Code != http.StatusServiceUnavailable || answerCode(rec) != "smtp_failed" {
			t.Fatalf("smtp down: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestKyCalendarSettings(t *testing.T) {
	f := newKyCalFixture(t)
	f.setEnabled(t, false)
	do := func(method, body string) map[string]any {
		req := httptest.NewRequest(method, "/api/settings/kycalendar", strings.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body.String())
		}
		return v
	}
	if v := do(http.MethodGet, ""); v["available"] != true || v["enabled"] != false {
		t.Fatalf("default: %v", v)
	}
	if v := do(http.MethodPut, `{"enabled":true}`); v["enabled"] != true {
		t.Fatalf("after PUT: %v", v)
	}
	f.srv.SetKyCalendar(nil)
	if v := do(http.MethodGet, ""); v["available"] != false {
		t.Fatalf("unpaired: %v", v)
	}
}

// Paired through KyIdentity, a user who never chose is on; any stored choice
// wins, including the false the earlier code wrote, which may be a deliberate
// opt-out. Paired by the environment, no choice stays off, as before.
func TestKyCalendarPerUserDefault(t *testing.T) {
	f := newKyCalFixture(t)
	base, _ := url.Parse("https://cal.example.com")
	env := kycalendar.New(base, kycalSecret, http.DefaultTransport)
	viaIdentity := kycalendar.NewKyIdentityForTest(base, kycalSecret, http.DefaultTransport)
	path := f.srv.userSettingsPath(f.userID)
	enabled := func() any {
		req := httptest.NewRequest(http.MethodGet, "/api/settings/kycalendar", nil)
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v["enabled"]
	}
	for i, tc := range []struct {
		file   string
		client *kycalendar.Client
		want   bool
		send   string
	}{
		{"", viaIdentity, true, "sent"},
		{"kyCalendar: false\n", viaIdentity, false, "sending_off"},
		{"kyCalendar: true\n", env, true, "sent"},
		{"", env, false, "sending_off"},
		{"kyCalendar: false\n", env, false, "sending_off"},
		{"notifications:\n  mode: none\n", viaIdentity, true, "sent"},
		{"kyCalendar: true\n", viaIdentity, true, "sent"},
	} {
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		f.srv.SetKyCalendar(tc.client)
		if got := enabled(); got != tc.want {
			t.Errorf("%d: %q via %v: enabled = %v", i, tc.file, tc.client.DefaultOn(), got)
		}
		rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, "ev-default-"+strconv.Itoa(i), sendBody(nil))
		if answerCode(rec) != tc.send {
			t.Errorf("%d: send = %d %s", i, rec.Code, rec.Body.String())
		}
		if tc.send == "sent" {
			<-f.delivered
		}
	}
	// A PUT stores the choice; an unrelated save keeps no choice as none.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.SetKyCalendar(viaIdentity)
	if err := config.UpdateUserSettings(path, func(s *config.UserSettings) error { s.EncryptIncoming = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "kyCalendar") || enabled() != true {
		t.Fatalf("an unrelated save recorded a choice: %s", raw)
	}
	f.setEnabled(t, false)
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "kyCalendar: false") || enabled() != false {
		t.Fatalf("explicit off not kept: %s", raw)
	}
}

// The admin status says how KyCalendar is paired, to administrators only,
// and never carries the secret.
func TestAdminKyCalendarStatus(t *testing.T) {
	f := newKyCalFixture(t)
	other, err := f.srv.users.Create(t.Context(), "bob", "pw-bob-testpassword", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	get := func(userID string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/kycalendar", nil)
		authRequestAs(f.srv, req, userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := get(f.userID); code != 200 || body != `{"source":"env","connected":true,"url":"https://cal.example.com"}`+"\n" {
		t.Fatalf("env status: %d %s", code, body)
	}
	if code, _ := get(other.ID); code != http.StatusForbidden {
		t.Fatalf("non-admin got %d", code)
	}
	f.srv.SetKyCalendar(nil)
	if code, body := get(f.userID); code != 200 || !strings.Contains(body, `"connected":false`) {
		t.Fatalf("unpaired status: %d %s", code, body)
	}
}

// The RSVP route forwards the invitation the server read from the message,
// never one the client sent, and maps KyCalendar's answers.
func TestCalendarRSVP(t *testing.T) {
	f := newKyCalFixture(t)
	var got []map[string]any
	answer := `{"outcome":"answered"}`
	status := http.StatusOK
	var receiver *kycalendar.Client
	cal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/api/itip/respond" {
			t.Errorf("path %s", r.URL.Path)
		}
		if _, err := receiver.Verify(r, kycalendar.EventRespond, body); err != nil {
			t.Errorf("verify: %v", err)
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		got = append(got, v)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer cal.Close()
	base, _ := url.Parse(cal.URL)
	receiver = kycalendar.New(base, kycalSecret, cal.Client().Transport)
	f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, cal.Client().Transport))
	f.fake.attachments = map[int][]mailmsg.Attachment{
		5: {{Name: "note.txt", MimeType: "text/plain", Content: []byte("hi")}, {Name: "invite.ics", MimeType: "text/calendar; method=REQUEST", Content: []byte(strings.ReplaceAll(inviteICS, "\r\n", "\n"))}},
	}
	rsvp := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/calendar/rsvp", strings.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		return rec
	}

	rec := rsvp(`{"messageId":"5","index":1,"partstat":"ACCEPTED","recurrenceId":"20261020T100000Z","ics":"EVIL"}`)
	if rec.Code != http.StatusOK || answerCode(rec) != "answered" || len(got) != 1 {
		t.Fatalf("rsvp: %d %s", rec.Code, rec.Body.String())
	}
	if got[0]["sub"] != "sub-1" || got[0]["issuer"] != kycalIssuer || got[0]["uid"] != "e1@example.com" ||
		got[0]["partstat"] != "ACCEPTED" || got[0]["recurrence_id"] != "20261020T100000Z" || got[0]["ics"] != inviteICS {
		t.Fatalf("forwarded %v", got[0])
	}

	status, answer = http.StatusNotFound, `{"error":"x","code":"no_event"}`
	if rec := rsvp(`{"messageId":"5","index":1,"partstat":"DECLINED"}`); rec.Code != http.StatusNotFound || answerCode(rec) != "no_event" {
		t.Fatalf("no_event: %d %s", rec.Code, rec.Body.String())
	}
	status, answer = http.StatusUnauthorized, `{"error":"Bad signature","code":"bad_signature"}`
	if rec := rsvp(`{"messageId":"5","index":1,"partstat":"DECLINED"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("kycalendar refused our signature: %d %s", rec.Code, rec.Body.String())
	}
	calls := len(got)
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"bad partstat":   {`{"messageId":"5","index":1,"partstat":"MAYBE"}`, 400, "bad_partstat"},
		"not a calendar": {`{"messageId":"5","index":0,"partstat":"ACCEPTED"}`, 422, "not_invitation"},
		"no attachment":  {`{"messageId":"5","index":7,"partstat":"ACCEPTED"}`, 404, "not_found"},
	} {
		if rec := rsvp(tc.body); rec.Code != tc.status || answerCode(rec) != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	f.setEnabled(t, false)
	if rec := rsvp(`{"messageId":"5","index":1,"partstat":"ACCEPTED"}`); rec.Code != http.StatusConflict || answerCode(rec) != "kycalendar_off" {
		t.Fatalf("setting off: %d %s", rec.Code, rec.Body.String())
	}
	if len(got) != calls {
		t.Fatalf("KyCalendar called for a refused RSVP")
	}
}
