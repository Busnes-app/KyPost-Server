package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
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

type flagTransport func(*http.Request) (*http.Response, error)

func (f flagTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func flagAction(t *testing.T, f *kycalFixture, action string, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": action, "messageIds": ids, "mailbox": "INBOX"})
	req := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", bytes.NewReader(body))
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("action: %d %s", rec.Code, rec.Body)
	}
	return rec
}
func TestLocalFlagsSurviveWithoutTaskSync(t *testing.T) {
	for _, mode := range []string{"switch off", "unpaired", "revoked link", "missing base URL"} {
		t.Run(mode, func(t *testing.T) {
			f := newKyCalFixture(t)
			f.srv.serverBaseURL = "https://mail.example.com"
			base, _ := url.Parse("https://cal.example.com")
			calls := 0
			f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, flagTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"outcome":"created"}`)), Header: make(http.Header)}, nil
			})))
			flagAction(t, f, "flag", []string{"5"})
			switch mode {
			case "switch off":
				f.setEnabled(t, false)
			case "unpaired":
				f.srv.SetKyCalendar(nil)
			case "revoked link":
				if err := f.srv.users.RevokeSSOLink(f.userID); err != nil {
					t.Fatal(err)
				}
			case "missing base URL":
				f.srv.serverBaseURL = ""
			}
			list := func() []kycalendar.Flag {
				flags, err := kycalendar.OpenFlags(f.srv.userStateDir(f.userID)).List()
				if err != nil {
					t.Fatal(err)
				}
				return flags
			}
			flagAction(t, f, "unflag", []string{"5"})
			if flags := list(); len(flags) != 1 || !flags[0].Cleared {
				t.Fatalf("stale unflag: %+v", flags)
			}
			flagAction(t, f, "flag", []string{"6"})
			flags := list()
			if len(flags) != 2 || flags[1].Cleared || flags[1].Message != "6" {
				t.Fatalf("local-only flag lost: %+v", flags)
			}
			if calls != 1 {
				t.Fatalf("task sync while unavailable: %d calls", calls)
			}
		})
	}
}
func TestBulkFlagTaskSyncSharesDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			f := newKyCalFixture(t)
			f.srv.serverBaseURL = "https://mail.example.com"
			base, _ := url.Parse("https://cal.example.com")
			calls := 0
			f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, flagTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if !timeout {
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})))
			start := time.Now()
			rec := flagAction(t, f, "flag", []string{"5", "6", "7"})
			if elapsed := time.Since(start); elapsed > flagTaskTimeout+2*time.Second {
				t.Errorf("task sync multiplied deadline: %s", elapsed)
			}
			if calls != 1 || len(f.fake.headerFields) != 1 {
				t.Errorf("continued network work: sends=%d headers=%v", calls, f.fake.headerFields)
			}
			var result struct {
				Processed int
				Tasks     map[string]string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Processed != 3 || len(result.Tasks) != 3 {
				t.Fatalf("lost actions: %s", rec.Body)
			}
			flags, err := kycalendar.OpenFlags(f.srv.userStateDir(f.userID)).List()
			if err != nil || len(flags) != 3 {
				t.Fatalf("lost local flags: %+v %v", flags, err)
			}

		})
	}
}

func TestTaskStateUnavailableMailboxRemainsRetryable(t *testing.T) {
	f := newKyCalFixture(t)
	u, err := f.srv.users.PublishPreparedSSOUser(t.Context(), "native-task-user", "native-task-user", users.RoleUser, kycalIssuer, "native-task-sub", "native-task-user", "native@example.test", func() (string, error) { return "native:" + strings.Repeat("ab", 32), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateUserSettings(f.srv.userSettingsPath(u.ID), func(s *config.UserSettings) error { on := true; s.KyCalendar = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	// Native mode is unavailable: authority cannot currently be admitted. This is not absence.
	ref, err := kycalendar.OpenFlags(f.srv.userStateDir(u.ID)).Put(kycalendar.Flag{Mailbox: "mbx-00000000-0000-4000-8000-000000000001", Folder: "INBOX", Message: "5"})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"sub": u.SSOSub, "issuer": kycalIssuer, "ref": ref, "done": true}
	for range 2 {
		rec := signedTo(t, f.srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, "retry-mailbox", body)
		if rec.Code != http.StatusServiceUnavailable || answerCode(rec) != "unavailable" {
			t.Fatalf("unavailable or replay swallowed: %d %s", rec.Code, rec.Body)
		}
	}
	flags, err := kycalendar.OpenFlags(f.srv.userStateDir(u.ID)).List()
	if err != nil || len(flags) != 1 || flags[0].Cleared {
		t.Fatalf("failure changed local mark: %+v %v", flags, err)
	}
}

func TestTaskStateUnknownMailboxIsGone(t *testing.T) {
	f := newKyCalFixture(t)
	ref, err := kycalendar.OpenFlags(f.srv.userStateDir(f.userID)).Put(kycalendar.Flag{Mailbox: "mbx-00000000-0000-4000-8000-000000000001", Folder: "INBOX", Message: "5"})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"sub": "sub-1", "issuer": kycalIssuer, "ref": ref, "done": true}
	rec := signedTo(t, f.srv, "/api/calendar/tasks/state", kycalendar.EventTaskState, "missing-mailbox", body)
	if rec.Code != http.StatusOK || answerCode(rec) != "gone" {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body)
	}
}

type unavailableFlagHeaders struct{ imapadapter.Client }

func (c unavailableFlagHeaders) FetchHeaderFields(context.Context, string, []int, ...string) (map[int][]string, error) {
	return nil, errors.New("temporary IMAP failure")
}
func TestFlagLinkHeaderFailureIsRetryable(t *testing.T) {
	f := newKyCalFixture(t)
	ref, err := kycalendar.OpenFlags(f.srv.userStateDir(f.userID)).Put(kycalendar.Flag{Folder: "INBOX", Message: "5"})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.userMail[f.userID].client = unavailableFlagHeaders{f.fake}
	req := httptest.NewRequest(http.MethodGet, "/api/mail/flags/"+ref, nil)
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "gone") {
		t.Fatalf("header failure: %d %s", rec.Code, rec.Body)
	}
}
