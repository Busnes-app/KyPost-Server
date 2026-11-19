package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func (f *scheduledFixture) sendDelay(t *testing.T, method string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/mail/send-delay", strings.NewReader(body))
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	return rec
}

func (f *scheduledFixture) setSendDelay(t *testing.T, seconds int) {
	t.Helper()
	if rec := f.sendDelay(t, http.MethodPut, `{"seconds":`+jsonInt(seconds)+`}`); rec.Code != http.StatusOK {
		t.Fatalf("set send delay %d: %d %s", seconds, rec.Code, rec.Body.String())
	}
}

func jsonInt(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// hold sends with the Send delay and returns the held row's ID and release time.
func (f *scheduledFixture) hold(t *testing.T, extra map[string]any) (string, time.Time) {
	t.Helper()
	body := map[string]any{"to": "bob@example.com", "subject": "held", "body": "hello held", "mode": "plain", "sendDelay": true}
	for k, v := range extra {
		body[k] = v
	}
	rec := f.post("/api/mail/send", body)
	var got struct {
		Delayed bool   `json:"delayed"`
		SendAt  string `json:"sendAt"`
	}
	id := scheduledID(t, rec)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	at, err := time.Parse(time.RFC3339, got.SendAt)
	if !got.Delayed || err != nil {
		t.Fatalf("held send answered %s", rec.Body.String())
	}
	return id, at
}

func TestSendDelaySetting(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	if rec := f.sendDelay(t, http.MethodGet, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"seconds":0}` {
		t.Fatalf("default: %d %s", rec.Code, rec.Body.String())
	}
	f.setSendDelay(t, 30)
	for _, body := range []string{`{"seconds":15}`, `{"seconds":-10}`, `{"seconds":30,"extra":true}`, `not json`} {
		if rec := f.sendDelay(t, http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := f.sendDelay(t, http.MethodGet, ""); strings.TrimSpace(rec.Body.String()) != `{"seconds":30}` {
		t.Fatalf("after refusals: %s", rec.Body.String())
	}
}

// Off, or a client that never asks for the delay, sends at once with the
// response it always had.
func TestSendDelayOffOrUnaskedSendsNow(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	rec := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "now", "sendDelay": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("delay off: %d %s", rec.Code, rec.Body.String())
	}
	f.setSendDelay(t, 30)
	rec = f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "now"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("client without the delay: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := f.srv.globalStore.ListScheduledSends(context.Background(), f.userID, f.userID)
	if _, data := f.smtp.counts(); data != 2 || len(rows) != 0 {
		t.Fatalf("%d delivered, %d held; want 2 and 0", data, len(rows))
	}
}

func TestSendDelayHoldsThenSends(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	f.setSendDelay(t, 30)
	pressed := time.Now()
	id, at := f.hold(t, nil)
	if d := at.Sub(pressed); d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("released %s after Send, want 30s", d)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/scheduled", nil)
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"delayed":true`) {
		t.Fatalf("list does not mark the hold: %s", rec.Body.String())
	}
	// Closing the tab changes nothing: the hold is a stored row, released by
	// the sweeper only once due.
	f.srv.sweepScheduledSends(context.Background(), clockAt(at.Add(-time.Second)))
	if dials, _ := f.smtp.counts(); dials != 0 || f.row(t, id).Status != state.ScheduledWaiting {
		t.Fatal("held message sent early")
	}
	f.srv.sweepScheduledSends(context.Background(), clockAt(at))
	if row := f.row(t, id); row.Status != "sent" {
		t.Fatalf("held message not released: %+v", row)
	}
	f.smtp.mu.Lock()
	defer f.smtp.mu.Unlock()
	msg, err := mail.ReadMessage(strings.NewReader(f.smtp.data[0]))
	if err != nil {
		t.Fatal(err)
	}
	if date, err := msg.Header.Date(); err != nil || date.Sub(pressed).Abs() > 2*time.Second {
		t.Fatalf("dated %v (%v), want when Send was pressed", date, err)
	}
}

// Cancel returns the message to Drafts intact, and it was never charged.
func TestSendDelayCancelRestoresDraftUncharged(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	setSendQuota(f.srv, 1, 100)
	f.setSendDelay(t, 10)
	id, _ := f.hold(t, map[string]any{"cc": "carol@example.com", "attachments": []map[string]string{{"name": "a.txt", "mimeType": "text/plain", "dataBase64": "aGk="}}})
	if rec := f.post("/api/scheduled/"+id+"/cancel", nil); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	assertReturned(t, f, id, reasonCanceled)
	d := f.fake.savedDrafts[0]
	if d.Subject != "held" || d.Body != "hello held" || len(d.CC) != 1 || len(d.Attachments) != 1 {
		t.Fatalf("draft not intact: %+v", d)
	}
	if code := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "now"}).Code; code != http.StatusOK {
		t.Fatalf("the canceled message used the quota: %d", code)
	}
	if code := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "again"}).Code; code != http.StatusTooManyRequests {
		t.Fatalf("quota not enforced: %d", code)
	}
}

// Cancel and release race on every iteration; exactly one wins. A message is
// either delivered and its cancel refused, or canceled into Drafts and never
// sent, never both and never neither.
func TestSendDelayCancelRacesRelease(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	f.setSendDelay(t, 10)
	for i := range 20 {
		id, at := f.hold(t, nil)
		_, before := f.smtp.counts()
		drafts := len(f.fake.savedDrafts)
		var wg sync.WaitGroup
		var rec *httptest.ResponseRecorder
		wg.Go(func() { rec = f.post("/api/scheduled/"+id+"/cancel", nil) })
		wg.Go(func() { f.srv.sweepScheduledSends(context.Background(), clockAt(at)) })
		wg.Wait()
		_, after := f.smtp.counts()
		sent, canceled := after-before, len(f.fake.savedDrafts)-drafts
		status := f.row(t, id).Status
		switch {
		case rec.Code == http.StatusOK && sent == 0 && canceled == 1 && status == "returned":
		case rec.Code == http.StatusConflict && sent == 1 && canceled == 0 && status == "sent":
		default:
			t.Fatalf("iteration %d: cancel %d, %d sent, %d drafts, row %s", i, rec.Code, sent, canceled, status)
		}
	}
}

// A scheduled message goes out at its time; the delay is not added on top.
func TestSendDelayNotAddedToScheduledSend(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	f.setSendDelay(t, 90)
	at := sendTime()
	id := f.schedule(t, at, map[string]any{"sendDelay": true})
	if row := f.row(t, id); row.SendAt != at.Unix() {
		t.Fatalf("scheduled for %d, want %d", row.SendAt, at.Unix())
	}
}

// A calendar reply answers at once whatever the delay.
func TestSendDelaySkipsCalendarReply(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	f.setSendDelay(t, 30)
	const ics = "BEGIN:VCALENDAR\nVERSION:2.0\nMETHOD:REPLY\nBEGIN:VEVENT\nUID:e1@x.test\nATTENDEE;PARTSTAT=ACCEPTED:mailto:alice@example.com\nEND:VEVENT\nEND:VCALENDAR\n"
	rec := f.post("/api/mail/send", map[string]any{"to": "organizer@x.test", "body": "b", "sendDelay": true, "calendarReply": map[string]string{"ics": ics}})
	if rec.Code != http.StatusOK {
		t.Fatalf("calendar reply: %d %s", rec.Code, rec.Body.String())
	}
}

// An unreadable setting refuses the send rather than sending at once.
func TestSendDelayUnreadableSettingFailsClosed(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	f.setSendDelay(t, 30)
	if err := os.WriteFile(f.srv.userSettingsPath(f.userID), []byte("sendDelay: 15\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "b", "sendDelay": true}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("send with an unreadable delay: %d %s", rec.Code, rec.Body.String())
	}
	if dials, _ := f.smtp.counts(); dials != 0 {
		t.Fatal("sent without its delay")
	}
}

// A browser-encrypted send holds its ciphertext; cancel discards it (the
// returned row keeps no sealed job) and files only the encrypted draftCopy.
func TestSendDelayHoldsClientPGP(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	u := convertedUser(t, f.srv)
	f.useAccount(t, u.ID)
	f.setSendDelay(t, 20)
	generation := uint64(1)
	delivery := scheduledPGPDelivery("alice@example.com", time.Now())
	send := func(req clientEncryptedSendRequest) *httptest.ResponseRecorder {
		req.Deliveries = []clientEncryptedDelivery{{Recipients: []string{"bob@example.com"}, Ciphertext: req.Deliveries[0].Ciphertext}}
		req.To, req.SentCopy, req.SentCopyEncrypted, req.MaterialGeneration, req.SendDelay = []string{"bob@example.com"}, delivery, true, &generation, true
		return f.post("/api/mail/send-pgp", req)
	}
	if rec := send(clientEncryptedSendRequest{Deliveries: []clientEncryptedDelivery{{Ciphertext: delivery}}}); rec.Code != http.StatusBadRequest {
		t.Errorf("held without a draft: %d %s", rec.Code, rec.Body.String())
	}
	// A browser clock five minutes off is no reason to refuse; an immediate
	// send never checked its Date either.
	skewed := scheduledPGPDelivery("alice@example.com", time.Now().Add(5*time.Minute))
	if rec := send(clientEncryptedSendRequest{Deliveries: []clientEncryptedDelivery{{Ciphertext: skewed}}, DraftCopy: skewed}); rec.Code != http.StatusAccepted {
		t.Errorf("held with a skewed Date: %d %s", rec.Code, rec.Body.String())
	}
	rec := send(clientEncryptedSendRequest{Deliveries: []clientEncryptedDelivery{{Ciphertext: delivery}}, DraftCopy: delivery})
	id := scheduledID(t, rec)
	if !strings.Contains(rec.Body.String(), `"delayed":true`) {
		t.Fatalf("not held: %s", rec.Body.String())
	}
	if rec := f.post("/api/scheduled/"+id+"/cancel", nil); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	assertReturned(t, f, id, reasonCanceled)
	if string(f.fake.savedDrafts[0].Raw) != strings.TrimSpace(delivery) {
		t.Fatal("returned draft is not the browser's encrypted draftCopy")
	}
}

// A tab that started the send as another account is refused before anything
// is built, sent or held.
func TestSendRefusesAnotherSessionsSend(t *testing.T) {
	f := newScheduledFixture(t, "250 queued")
	for _, path := range []string{"/api/mail/send", "/api/mail/send-pgp"} {
		raw, _ := json.Marshal(map[string]any{"to": "bob@example.com", "body": "b"})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
		authRequestAs(f.srv, req, f.userID)
		req.Header.Set("X-KyPost-User", "someone-else")
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"sessionChanged":true`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if code := f.post("/api/mail/send", map[string]any{"to": "bob@example.com", "body": "b"}).Code; code != http.StatusOK {
		t.Fatalf("own send: %d", code)
	}
	if _, data := f.smtp.counts(); data != 1 {
		t.Fatalf("%d delivered, want only the own send", data)
	}
}
