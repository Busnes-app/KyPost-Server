package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var pulseSecret = []byte(strings.Repeat("p", suitepair.MinSecretBytes))

// sentRecorder is a mail client that keeps every Sent copy.
type sentRecorder struct {
	*fakeMailClient
	mu   sync.Mutex
	sent [][]byte
}

func (r *sentRecorder) SaveSent(_ context.Context, d imapadapter.DraftMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, d.Raw)
	return nil
}

func (r *sentRecorder) copies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte{}, r.sent...)
}

type pulseFixture struct {
	*kycalFixture
	sent *sentRecorder
}

// newPulseFixture is newKyCalFixture's user (alice@example.com, alias
// team@example.com) paired with KyPulse, switch on, ops@x.test confirmed and
// pending@x.test listed but unconfirmed.
func newPulseFixture(t *testing.T) *pulseFixture {
	t.Helper()
	f := &pulseFixture{kycalFixture: newKyCalFixture(t), sent: &sentRecorder{fakeMailClient: &fakeMailClient{}}}
	f.srv.pairingSecret = "kypulse-test-pairing-secret-32-bytes-long"
	f.srv.userMu.Lock()
	f.srv.userMail[f.userID] = &serverMailEntry{client: f.sent, updatedAt: "test"}
	f.srv.userMu.Unlock()
	f.srv.SetKyPulse(suitepair.New(KyPulsePeer, nil, pulseSecret, nil))
	if err := config.UpdateUserSettings(f.srv.userSettingsPath(f.userID), func(s *config.UserSettings) error {
		s.KyPulse = config.KyPulseSettings{Enabled: true, Recipients: []config.KyPulseRecipient{
			{Address: "ops@x.test", Confirmed: true}, {Address: "pending@x.test", Nonce: "n1"},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *pulseFixture) setPulse(t *testing.T, on bool) {
	t.Helper()
	if err := config.UpdateUserSettings(f.srv.userSettingsPath(f.userID), func(s *config.UserSettings) error {
		s.KyPulse.Enabled = on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func pulseSend(t *testing.T, srv *Server, secret []byte, eventType, eventID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	h, err := syncauth.Sign(secret, time.Now(), eventType, eventID, raw)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/suite/kypulse/alert", bytes.NewReader(raw))
	h.Apply(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func alertBody(mutate func(map[string]any)) map[string]any {
	b := map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "recipients": []string{"ops@x.test", "alice@example.com"},
		"subject": "KyPost is down", "text": "KyPulse: KyPost is down (refused)\nhttps://pulse.example/#/apps/tgt_1"}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func (f *pulseFixture) delivered(t *testing.T) string {
	t.Helper()
	select {
	case raw := <-f.kycalFixture.delivered:
		return raw
	case <-time.After(smtpWaitTime):
		t.Fatal("nothing delivered")
		return ""
	}
}

// Every refusal answers its contract code and sends nothing.
func TestKyPulseAlertRefusals(t *testing.T) {
	set := func(k string, v any) func(map[string]any) { return func(b map[string]any) { b[k] = v } }
	for name, tc := range map[string]struct {
		setup     func(*testing.T, *pulseFixture)
		secret    []byte
		eventType string
		body      any
		status    int
		code      string
	}{
		"wrong secret":       {secret: []byte(strings.Repeat("x", 32)), body: alertBody(nil), status: 401, code: "bad_signature"},
		"wrong event type":   {eventType: "kycalendar.itip.send", body: alertBody(nil), status: 401, code: "bad_signature"},
		"malformed json":     {body: []byte("{"), status: 400, code: "bad_body"},
		"trailing data":      {body: []byte(`{"sub":"sub-1"} {}`), status: 400, code: "bad_body"},
		"html":               {body: alertBody(set("html", "<b>x</b>")), status: 400, code: "bad_body"},
		"reply_to":           {body: alertBody(set("reply_to", "evil@x.test")), status: 400, code: "bad_body"},
		"attachments":        {body: alertBody(set("attachments", []string{"x"})), status: 400, code: "bad_body"},
		"headers":            {body: alertBody(set("headers", map[string]string{"Bcc": "evil@x.test"})), status: 400, code: "bad_body"},
		"from":               {body: alertBody(set("from", "team@example.com")), status: 400, code: "bad_body"},
		"missing text":       {body: alertBody(func(b map[string]any) { delete(b, "text") }), status: 400, code: "bad_body"},
		"long subject":       {body: alertBody(set("subject", strings.Repeat("s", 151))), status: 400, code: "bad_body"},
		"subject newline":    {body: alertBody(set("subject", "a\nBcc: evil@x.test")), status: 400, code: "bad_body"},
		"long text":          {body: alertBody(set("text", strings.Repeat("t", 8<<10+1))), status: 400, code: "bad_body"},
		"text CR":            {body: alertBody(set("text", "a\r\nb")), status: 400, code: "bad_body"},
		"oversize body":      {body: alertBody(set("text", strings.Repeat("t", 17<<10))), status: 400, code: "bad_body"},
		"no recipients":      {body: alertBody(set("recipients", []string{})), status: 400, code: "bad_body"},
		"twelve recipients":  {body: alertBody(set("recipients", strings.Split("a@x.test b@x.test c@x.test d@x.test e@x.test f@x.test g@x.test h@x.test i@x.test j@x.test k@x.test l@x.test", " "))), status: 400, code: "bad_body"},
		"duplicate":          {body: alertBody(set("recipients", []string{"ops@x.test", "OPS@x.test"})), status: 400, code: "bad_body"},
		"named recipient":    {body: alertBody(set("recipients", []string{"Ops <ops@x.test>"})), status: 400, code: "bad_body"},
		"unknown subject":    {body: alertBody(set("sub", "sub-2")), status: 404, code: "no_user"},
		"other issuer":       {body: alertBody(set("issuer", "https://evil.example")), status: 404, code: "no_user"},
		"switch off":         {setup: func(t *testing.T, f *pulseFixture) { f.setPulse(t, false) }, body: alertBody(nil), status: 409, code: "alerts_off"},
		"not on list":        {body: alertBody(set("recipients", []string{"ops@x.test", "mallory@x.test"})), status: 409, code: "recipient_not_allowed"},
		"unconfirmed":        {body: alertBody(set("recipients", []string{"pending@x.test"})), status: 409, code: "recipient_unconfirmed"},
		"not allowed first":  {body: alertBody(set("recipients", []string{"pending@x.test", "mallory@x.test"})), status: 409, code: "recipient_not_allowed"},
		"dedicated admin":    {setup: dedicatedAdmin, body: alertBody(nil), status: 404, code: "no_user"},
		"revoked link":       {setup: revokeLink, body: alertBody(nil), status: 404, code: "no_user"},
		"no outgoing server": {setup: noSMTP, body: alertBody(set("recipients", []string{"ops@x.test"})), status: 409, code: "no_mailbox"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPulseFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			secret := pulseSecret
			if tc.secret != nil {
				secret = tc.secret
			}
			eventType := kypulseEventAlert
			if tc.eventType != "" {
				eventType = tc.eventType
			}
			rec := pulseSend(t, f.srv, secret, eventType, "kypulse-alert-"+name, tc.body)
			if rec.Code != tc.status || answerCode(rec) != tc.code {
				t.Fatalf("status %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			noDelivery(t, f.kycalFixture, name)
			if len(f.sent.copies()) != 0 {
				t.Fatal("a refused alert was filed in Sent")
			}
		})
	}
}

// dedicatedAdmin makes the fixture's administrator a dedicated
// administrator identity: native mail, where admins own no mailbox.
func dedicatedAdmin(t *testing.T, f *pulseFixture) {
	t.Helper()
	if u, err := f.srv.users.Get(f.userID); err != nil || u.Role != users.RoleAdmin {
		t.Fatalf("fixture user: %v %v", u.Role, err)
	}
	f.srv.nativeMail = true
}

func revokeLink(t *testing.T, f *pulseFixture) {
	t.Helper()
	if err := f.srv.users.RevokeSSOLink(f.userID); err != nil {
		t.Fatal(err)
	}
}

func noSMTP(t *testing.T, f *pulseFixture) {
	t.Helper()
	if err := os.Remove(f.srv.userIMAPConfigPath(f.userID)); err != nil {
		t.Fatal(err)
	}
}

func TestKyPulseAlertOffWhenUnpaired(t *testing.T) {
	f := newPulseFixture(t)
	f.srv.SetKyPulse(nil)
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-1", alertBody(nil)); rec.Code != 404 || answerCode(rec) != "kypulse_off" {
		t.Fatalf("unpaired: %d %s", rec.Code, rec.Body.String())
	}
	f.srv.SetKyPulse(suitepair.FromKyIdentity(KyPulsePeer))
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-2", alertBody(nil)); rec.Code != 404 || answerCode(rec) != "kypulse_off" {
		t.Fatalf("not yet resolved: %d %s", rec.Code, rec.Body.String())
	}
	noDelivery(t, f.kycalFixture, "unpaired")
}

// An alert reaches the owner's SMTP server as the fixed template and is filed
// in Sent; a replay is answered duplicate without sending again.
func TestKyPulseAlertDelivers(t *testing.T) {
	f := newPulseFixture(t)
	rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "kypulse-alert-ev1", alertBody(nil))
	if rec.Code != http.StatusAccepted || answerCode(rec) != "sent" {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	raw := f.delivered(t)
	checkPulseMessage(t, raw, "[KyPulse] KyPost is down", "ops@x.test, alice@example.com")
	msg, _ := mail.ReadMessage(strings.NewReader(raw))
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(msg.Body)
	text, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.String(), "\r\n", ""))
	if string(text) != "KyPulse: KyPost is down (refused)\r\nhttps://pulse.example/#/apps/tgt_1" {
		t.Fatalf("text = %q", text)
	}
	copies := f.sent.copies()
	if len(copies) != 1 {
		t.Fatalf("Sent copies = %d", len(copies))
	}
	checkPulseMessage(t, string(copies[0]), "[KyPulse] KyPost is down", "ops@x.test, alice@example.com")

	again := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "kypulse-alert-ev1", alertBody(nil))
	if again.Code != http.StatusOK || answerCode(again) != "duplicate" {
		t.Fatalf("replay: %d %s", again.Code, again.Body.String())
	}
	noDelivery(t, f.kycalFixture, "replay")
}

func checkPulseMessage(t *testing.T, raw, subject, to string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name := range msg.Header {
		switch name {
		case "From", "To", "Subject", "Date", "Message-Id", "Mime-Version", "Content-Type", "Content-Transfer-Encoding", "Auto-Submitted", "X-Ky-Source":
		default:
			t.Errorf("unexpected header %s", name)
		}
	}
	h := msg.Header
	if h.Get("From") != "alice@example.com" || h.Get("To") != to || h.Get("Subject") != subject ||
		h.Get("Auto-Submitted") != "auto-generated" || h.Get("X-Ky-Source") != "kypulse" ||
		h.Get("Content-Type") != "text/plain; charset=UTF-8" || h.Get("Reply-To") != "" {
		t.Fatalf("headers: %v", h)
	}
}

// A refusal forgets the event ID, so KyPulse's retry runs again.
func TestKyPulseAlertRetryAfterRefusal(t *testing.T) {
	f := newPulseFixture(t)
	f.setPulse(t, false)
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-retry", alertBody(nil)); answerCode(rec) != "alerts_off" {
		t.Fatalf("first: %s", rec.Body.String())
	}
	f.setPulse(t, true)
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-retry", alertBody(nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	f.delivered(t)
}

// The 31st alert in an hour is refused with Retry-After; a retry of an
// accepted alert does not count twice.
func TestKyPulseAlertRateLimit(t *testing.T) {
	f := newPulseFixture(t)
	if _, err := f.srv.users.SetSendQuota(f.userID, &config.SendQuota{MessagesPerHour: 100, RecipientsPerDay: 1000}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := range kypulseAlertsPerHour - 1 {
		if _, err := f.srv.globalStore.AdmitSend(t.Context(), "kypulse-rate:"+f.userID+":pre-"+strconv.Itoa(i), "kypulse:"+f.userID, 1, kypulseAlertsPerHour, kypulseAlertsPerHour*24, now); err != nil {
			t.Fatal(err)
		}
	}
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-30", alertBody(nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("30th: %d %s", rec.Code, rec.Body.String())
	}
	f.delivered(t)
	rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-31", alertBody(nil))
	if rec.Code != http.StatusTooManyRequests || answerCode(rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("31st: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	noDelivery(t, f.kycalFixture, "31st")
}

func (f *pulseFixture) settings(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	return rec
}

var confirmLink = regexp.MustCompile(`/kypulse/confirm\?t=([A-Za-z0-9_.-]+)`)

// confirmToken reads the link out of a delivered confirmation mail, which
// carries the same fixed template as an alert.
func confirmToken(t *testing.T, raw, to string) string {
	t.Helper()
	checkPulseMessage(t, raw, "[KyPulse] Confirm alert mail", to)
	msg, _ := mail.ReadMessage(strings.NewReader(raw))
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(msg.Body)
	text, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.String(), "\r\n", ""))
	if !strings.Contains(string(text), "from alice@example.com to this address") {
		t.Fatalf("confirmation text: %s", text)
	}
	m := confirmLink.FindStringSubmatch(string(text))
	if m == nil {
		t.Fatalf("no link in %s", text)
	}
	return m[1]
}

func (f *pulseFixture) confirm(t *testing.T, method, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/kypulse/confirm?t="+url.QueryEscape(token), nil)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	return rec
}

func (f *pulseFixture) recipients(t *testing.T) map[string]bool {
	t.Helper()
	rec := f.settings(t, http.MethodGet, "/api/settings/kypulse", "")
	var v struct {
		Available, Enabled bool
		Own                []string
		Recipients         []struct {
			Address   string
			Confirmed bool
		}
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	out := map[string]bool{}
	for _, x := range v.Recipients {
		out[x.Address] = x.Confirmed
	}
	return out
}

func TestKyPulseSettings(t *testing.T) {
	f := newPulseFixture(t)
	f.setPulse(t, false)
	rec := f.settings(t, http.MethodGet, "/api/settings/kypulse", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"available":true,"enabled":false,"own":["alice@example.com","team@example.com"]`) ||
		!strings.Contains(rec.Body.String(), `{"address":"ops@x.test","confirmed":true}`) || strings.Contains(rec.Body.String(), "nonce") {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.settings(t, http.MethodPut, "/api/settings/kypulse", `{"enabled":true}`); !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	for name, tc := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"own address":   {"/api/settings/kypulse/recipients", `{"address":"Team@example.com"}`, 409, "own_address"},
		"duplicate":     {"/api/settings/kypulse/recipients", `{"address":"ops@x.test"}`, 409, "duplicate"},
		"named":         {"/api/settings/kypulse/recipients", `{"address":"Ops <new@x.test>"}`, 400, "bad_address"},
		"resend absent": {"/api/settings/kypulse/recipients/resend", `{"address":"nobody@x.test"}`, 404, "not_listed"},
		"resend done":   {"/api/settings/kypulse/recipients/resend", `{"address":"ops@x.test"}`, 409, "already_confirmed"},
	} {
		if rec := f.settings(t, http.MethodPost, tc.path, tc.body); rec.Code != tc.status || answerCode(rec) != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if err := config.UpdateUserSettings(f.srv.userSettingsPath(f.userID), func(s *config.UserSettings) error {
		for i := len(s.KyPulse.Recipients); i < config.MaxKyPulseRecipients; i++ {
			s.KyPulse.Recipients = append(s.KyPulse.Recipients, config.KyPulseRecipient{Address: "r" + strconv.Itoa(i) + "@x.test", Confirmed: true})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.settings(t, http.MethodPost, "/api/settings/kypulse/recipients", `{"address":"eleventh@x.test"}`); rec.Code != 409 || answerCode(rec) != "list_full" {
		t.Fatalf("eleventh: %d %s", rec.Code, rec.Body.String())
	}
	noDelivery(t, f.kycalFixture, "refused list changes")
	f.srv.SetKyPulse(nil)
	if rec := f.settings(t, http.MethodGet, "/api/settings/kypulse", ""); !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Fatalf("unpaired: %s", rec.Body.String())
	}
}

// Opening the link confirms nothing; the POST behind its button confirms
// once. A resend kills the older link, removal kills the live one, and a
// forged, foreign-purpose or expired token confirms nothing.
func TestKyPulseConfirmLink(t *testing.T) {
	f := newPulseFixture(t)
	if rec := f.settings(t, http.MethodPost, "/api/settings/kypulse/recipients", `{"address":"New@X.test"}`); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	token := confirmToken(t, f.delivered(t), "new@x.test")
	if len(f.sent.copies()) != 1 {
		t.Fatal("confirmation mail not filed in Sent")
	}
	if rec := f.settings(t, http.MethodPost, "/api/settings/kypulse/recipients/resend", `{"address":"new@x.test"}`); rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("resend inside the cooldown: %d %s", rec.Code, rec.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rec := f.confirm(t, method, token); rec.Code != 200 {
			t.Fatalf("%s page: %d", method, rec.Code)
		}
	}
	if page := f.confirm(t, http.MethodGet, token).Body.String(); !strings.Contains(page, `method="post"`) || strings.Contains(page, "new@x.test") || strings.Contains(page, "alice") {
		t.Fatalf("page: %s", page)
	}
	if f.recipients(t)["new@x.test"] {
		t.Fatal("GET confirmed the address")
	}
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-pending", alertBody(func(b map[string]any) { b["recipients"] = []string{"new@x.test"} })); answerCode(rec) != "recipient_unconfirmed" {
		t.Fatalf("pending alert: %s", rec.Body.String())
	}
	for name, bad := range map[string]string{
		"tampered":      token[:len(token)-2] + "xx",
		"empty":         "",
		"other purpose": f.mintToken(t, pairingPurposePickupLink, time.Hour, strings.SplitN(f.tokenSub(t, token), "|", 2)[0]),
		"expired":       f.mintToken(t, pairingPurposeKyPulse, -time.Minute, f.tokenSub(t, token)),
	} {
		if rec := f.confirm(t, http.MethodPost, bad); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if f.recipients(t)["new@x.test"] {
		t.Fatal("a bad token confirmed the address")
	}
	if rec := f.confirm(t, http.MethodPost, token); rec.Code != 200 || !f.recipients(t)["new@x.test"] {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.confirm(t, http.MethodPost, token); rec.Code != http.StatusGone {
		t.Fatalf("used link: %d", rec.Code)
	}
	if rec := pulseSend(t, f.srv, pulseSecret, kypulseEventAlert, "ev-new", alertBody(func(b map[string]any) { b["recipients"] = []string{"new@x.test"} })); rec.Code != http.StatusAccepted {
		t.Fatalf("confirmed alert: %s", rec.Body.String())
	}
	f.delivered(t)

	f.srv.kypulseConfirmCooldown = nil
	if rec := f.settings(t, http.MethodPost, "/api/settings/kypulse/recipients", `{"address":"second@x.test"}`); rec.Code != 200 {
		t.Fatalf("add second: %d %s", rec.Code, rec.Body.String())
	}
	old := confirmToken(t, f.delivered(t), "second@x.test")
	if rec := f.settings(t, http.MethodPost, "/api/settings/kypulse/recipients/resend", `{"address":"second@x.test"}`); rec.Code != 200 {
		t.Fatalf("resend: %d %s", rec.Code, rec.Body.String())
	}
	fresh := confirmToken(t, f.delivered(t), "second@x.test")
	if rec := f.confirm(t, http.MethodPost, old); rec.Code != http.StatusGone || f.recipients(t)["second@x.test"] {
		t.Fatalf("link before resend: %d", rec.Code)
	}
	if rec := f.settings(t, http.MethodDelete, "/api/settings/kypulse/recipients", `{"address":"second@x.test"}`); rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.confirm(t, http.MethodPost, fresh); rec.Code != http.StatusGone {
		t.Fatalf("link after removal: %d", rec.Code)
	}
	if _, listed := f.recipients(t)["second@x.test"]; listed {
		t.Fatal("removed address still listed")
	}
	if rec := f.settings(t, http.MethodDelete, "/api/settings/kypulse/recipients", `{"address":"second@x.test"}`); rec.Code != 404 {
		t.Fatalf("remove twice: %d", rec.Code)
	}
}

func (f *pulseFixture) tokenSub(t *testing.T, token string) string {
	t.Helper()
	claims, err := f.srv.decodeAndVerifyPairingToken(token, pairingPurposeKyPulse, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return claims.Sub
}

// mintToken signs claims with the server's pairing secret, at any expiry.
func (f *pulseFixture) mintToken(t *testing.T, purpose string, ttl time.Duration, sub string) string {
	t.Helper()
	payload, _ := json.Marshal(pairingTokenClaims{Sub: sub, Exp: time.Now().Add(ttl).Unix(), Nonce: "n", Purpose: purpose})
	mac := hmac.New(sha256.New, []byte(f.srv.pairingSecret))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
