package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
)

// signedTo posts body to path, signed as eventType under secret.
func signedTo(t *testing.T, srv *Server, path, eventType, eventID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	h, err := syncauth.Sign(kycalSecret, time.Now(), eventType, eventID, raw)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	h.Apply(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// Flagging a message makes a KyCalendar task carrying the subject, a link and an opaque
// reference only; KyCalendar's task state sets and clears the flag back.
func TestFlagMakesKyCalendarTask(t *testing.T) {
	f := newKyCalFixture(t)
	f.srv.serverBaseURL = "https://mail.example.com/"
	var got []map[string]any
	answer, status := `{"outcome":"created","calendar_id":"c","uid":"u"}`, http.StatusOK
	var receiver *kycalendar.Client
	cal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/api/tasks/flag" {
			t.Errorf("path %s", r.URL.Path)
		}
		if _, err := receiver.Verify(r, kycalendar.EventTaskFlag, body); err != nil {
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
	f.fake.headerLines = map[int][]string{5: {"Subject: =?UTF-8?Q?Caf=C3=A9_plans?=\x01 \tnow"}}
	act := func(body string) (*httptest.ResponseRecorder, map[string]string) {
		req := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", strings.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v struct{ Tasks map[string]string }
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec, v.Tasks
	}
	flags := func() []map[string]string {
		req := httptest.NewRequest(http.MethodGet, "/api/mail/flags", nil)
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v struct{ Flags []map[string]string }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("flags: %d %s", rec.Code, rec.Body.String())
		}
		return v.Flags
	}

	rec, tasks := act(`{"action":"flag","messageIds":["5"],"mailbox":"INBOX","due":"2026-11-20"}`)
	if rec.Code != http.StatusOK || tasks["5"] != "created" || len(got) != 1 {
		t.Fatalf("flag: %d %s", rec.Code, rec.Body.String())
	}
	ref, _ := got[0]["ref"].(string)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`).MatchString(ref) || got[0]["sub"] != "sub-1" || got[0]["issuer"] != kycalIssuer ||
		got[0]["subject"] != "Café plans now" || got[0]["url"] != "https://mail.example.com/read?flag="+ref ||
		got[0]["due"] != "2026-11-20" || got[0]["flagged"] != true || len(got[0]) != 7 {
		t.Fatalf("sent %v", got[0])
	}
	if !slices.Contains(f.fake.inboxActions, "flag:5:INBOX") {
		t.Fatalf("not flagged in the mailbox: %v", f.fake.inboxActions)
	}
	if l := flags(); len(l) != 1 || l[0]["ref"] != ref || l[0]["messageId"] != "5" || l[0]["folder"] != "INBOX" || l[0]["due"] != "2026-11-20" {
		t.Fatalf("flags %v", l)
	}
	// The task's link opens the message, and says so when it is gone.
	link := func(r string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/mail/flags/"+r, nil)
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := link(ref); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"messageId":"5"`) || !strings.Contains(rec.Body.String(), `"folder":"INBOX"`) {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := link("unknown"); rec.Code != http.StatusNotFound || answerCode(rec) != "gone" {
		t.Fatalf("unknown link: %d %s", rec.Code, rec.Body.String())
	}
	lines := f.fake.headerLines
	f.fake.headerLines = map[int][]string{}
	if rec := link(ref); rec.Code != http.StatusNotFound || answerCode(rec) != "gone" {
		t.Fatalf("deleted message: %d %s", rec.Code, rec.Body.String())
	}
	f.fake.headerLines = lines
	// Flagging again names the same task; a due date needs a flag and a real date.
	answer = `{"outcome":"updated"}`
	if _, tasks := act(`{"action":"flag","messageIds":["5"],"mailbox":"INBOX"}`); tasks["5"] != "updated" || got[1]["ref"] != ref || got[1]["due"] != nil {
		t.Fatalf("flag again: %v %v", tasks, got[1])
	}
	for _, body := range []string{`{"action":"flag","messageIds":["5"],"due":"tomorrow"}`, `{"action":"unflag","messageIds":["5"],"due":"2026-11-20"}`} {
		if rec, _ := act(body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	// Unflag completes the task under the same reference.
	answer = `{"outcome":"completed"}`
	if _, tasks := act(`{"action":"unflag","messageIds":["5"],"mailbox":"INBOX"}`); tasks["5"] != "completed" || got[2]["ref"] != ref || got[2]["flagged"] != false {
		t.Fatalf("unflag: %v %v", tasks, got[2])
	}
	if l := flags(); len(l) != 0 {
		t.Fatalf("cleared flag still listed: %v", l)
	}
	// Unflagging a message KyCalendar never heard of calls nobody.
	if _, tasks := act(`{"action":"unflag","messageIds":["9"],"mailbox":"INBOX"}`); tasks["9"] != "none" || len(got) != 3 {
		t.Fatalf("unflag unknown: %v", tasks)
	}

	// KyCalendar's task state: reopening flags again, completing clears, repeats change nothing.
	calls := len(f.fake.inboxActions)
	state := func(id string, body map[string]any) *httptest.ResponseRecorder {
		return signedTo(t, f.srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, id, body)
	}
	body := func(done any) map[string]any {
		return map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "ref": ref, "done": done}
	}
	if rec := state("s1", body(false)); rec.Code != http.StatusOK || answerCode(rec) != "flagged" || f.fake.inboxActions[calls] != "flag:5:INBOX" || len(flags()) != 1 {
		t.Fatalf("reopen: %d %s %v", rec.Code, rec.Body.String(), f.fake.inboxActions)
	}
	if rec := state("s1", body(false)); answerCode(rec) != "duplicate" {
		t.Fatalf("replay: %s", rec.Body.String())
	}
	if rec := state("s2", body(true)); answerCode(rec) != "cleared" || f.fake.inboxActions[calls+1] != "unflag:5:INBOX" || len(flags()) != 0 {
		t.Fatalf("done: %s %v", rec.Body.String(), f.fake.inboxActions)
	}
	if rec := state("s3", body(true)); answerCode(rec) != "unchanged" || len(f.fake.inboxActions) != calls+2 {
		t.Fatalf("done again: %s", rec.Body.String())
	}
	other := body(true)
	other["ref"] = "nope"
	if rec := state("s4", other); answerCode(rec) != "gone" {
		t.Fatalf("unknown ref: %s", rec.Body.String())
	}
	if rec := signedTo(t, f.srv, "/api/calendar/tasks/state", kycalendar.EventTaskFlag, "s5", body(true)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reflected event type: %d", rec.Code)
	}
	stranger := body(true)
	stranger["sub"] = "sub-2"
	if rec := state("s6", stranger); rec.Code != http.StatusNotFound || answerCode(rec) != "no_user" {
		t.Fatalf("stranger: %d %s", rec.Code, rec.Body.String())
	}
	if rec := state("s7", map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "ref": ref}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no done: %d", rec.Code)
	}

	// KyCalendar failing leaves the flag set and says so.
	answer, status = `{"error":"x","code":"error"}`, http.StatusInternalServerError
	before := len(f.fake.inboxActions)
	if rec, tasks := act(`{"action":"flag","messageIds":["5"],"mailbox":"INBOX"}`); rec.Code != http.StatusOK || tasks["5"] != "failed" || len(f.fake.inboxActions) != before+1 {
		t.Fatalf("kycalendar down: %d %v", rec.Code, tasks)
	}
	// The user's switch off: flags stay in KyPost, nobody is called, the state route refuses.
	f.setEnabled(t, false)
	n := len(got)
	if rec, tasks := act(`{"action":"flag","messageIds":["5"],"mailbox":"INBOX"}`); rec.Code != http.StatusOK || len(tasks) != 0 || len(got) != n {
		t.Fatalf("switch off: %d %v", rec.Code, tasks)
	}
	if rec := state("s8", body(false)); rec.Code != http.StatusConflict || answerCode(rec) != "sending_off" {
		t.Fatalf("state with switch off: %d %s", rec.Code, rec.Body.String())
	}
}

// Unpaired, the state route is absent and a flag is only a flag.
func TestTaskStateOffWhenUnpaired(t *testing.T) {
	srv := newTestServer(t)
	rec := signedTo(t, srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, "e", map[string]any{"sub": "s", "issuer": "i", "ref": "r", "done": true})
	if rec.Code != http.StatusNotFound || answerCode(rec) != "kycalendar_off" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
