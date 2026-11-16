package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
)

// Every save sends KyCalendar the whole state, signed as kypost.ooo.set: away
// only with both dates and the box ticked, all-day when both are midnight in
// the zone, the remote comment only while the remote reply goes to everyone.
func TestOutOfOfficeSyncsKyCalendar(t *testing.T) {
	f := newKyCalFixture(t)
	f.srv.cfg.OutOfOfficeRemote = true
	var got []map[string]any
	status := http.StatusOK
	var receiver *kycalendar.Client
	cal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := receiver.Verify(r, kycalendar.EventOOO, body); err != nil || r.URL.Path != "/api/kypost/out-of-office" {
			t.Errorf("KyCalendar got %s: %v", r.URL.Path, err)
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		got = append(got, v)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	defer cal.Close()
	base, _ := url.Parse(cal.URL)
	receiver = kycalendar.New(base, kycalSecret, cal.Client().Transport)
	f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, cal.Client().Transport))
	put := func(body string) outOfOfficeView {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/mail/out-of-office", strings.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v outOfOfficeView
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
		}
		return v
	}

	v := put(`{"enabled":true,"start":"2026-12-01T05:00:00Z","end":"2026-12-05T05:00:00Z","timeZone":"America/New_York","localMessage":"Away","remote":"on","remoteMessage":"Out","calendarAway":true,"calendarTentative":true}`)
	if v.CalendarSync != "synced" || !v.CalendarAvailable || len(got) != 1 {
		t.Fatalf("sync %q, sent %d", v.CalendarSync, len(got))
	}
	b := got[0]
	if b["sub"] != "sub-1" || b["issuer"] != kycalIssuer || b["away"] != true || b["all_day"] != true || b["time_zone"] != "America/New_York" ||
		b["tentative"] != true || b["comment_local"] != "Away" || b["comment_remote"] != "Out" || b["revision"].(float64) <= 0 {
		t.Fatalf("sent %v", b)
	}
	// Contacts only: no remote comment. Timed, not all-day.
	put(`{"enabled":true,"start":"2026-12-01T09:00:00Z","end":"2026-12-01T17:00:00Z","timeZone":"UTC","localMessage":"Away","remote":"contacts","remoteMessage":"Out","calendarAway":true}`)
	if b := got[1]; b["comment_remote"] != nil || b["all_day"] != nil || b["tentative"] != false {
		t.Fatalf("contacts only sent %v", b)
	}
	// Box unticked, no end date, or the reply off: away false, nothing else.
	for _, body := range []string{
		`{"enabled":true,"start":"2026-12-01T09:00:00Z","end":"2026-12-02T09:00:00Z","localMessage":"Away","calendarAway":false}`,
		`{"enabled":true,"start":"2026-12-01T09:00:00Z","localMessage":"Away","calendarAway":true}`,
		`{"enabled":false,"start":"2026-12-01T09:00:00Z","end":"2026-12-02T09:00:00Z","localMessage":"Away","calendarAway":true}`,
	} {
		put(body)
		if b := got[len(got)-1]; b["away"] != false || b["start"] != nil || b["comment_local"] != nil {
			t.Fatalf("%s sent %v", body, b)
		}
	}
	// KyCalendar refusing is reported; the save stands.
	status = http.StatusInternalServerError
	if v := put(`{"enabled":false}`); v.CalendarSync != "failed" {
		t.Fatalf("failure reported as %q", v.CalendarSync)
	}
	// The user's KyCalendar switch off: nothing is sent.
	status = http.StatusOK
	f.setEnabled(t, false)
	n := len(got)
	if v := put(`{"enabled":false}`); v.CalendarSync != "" || v.CalendarAvailable || len(got) != n {
		t.Fatalf("switch off: %q, sent %d", v.CalendarSync, len(got)-n)
	}
}

// After the administrator stops outside replies, KyCalendar is sent the state
// again without the remote comment, and a REPLY it still asks to mail with the
// comment synced before goes out without it. Other comments are kept.
func TestOutOfOfficeRemoteCommentRevoked(t *testing.T) {
	f := newKyCalFixture(t)
	f.srv.cfgMu.Lock()
	f.srv.cfg.OutOfOfficeRemote = true
	f.srv.cfgMu.Unlock()
	var mu sync.Mutex
	var got []map[string]any
	cal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &v)
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	defer cal.Close()
	base, _ := url.Parse(cal.URL)
	f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, cal.Client().Transport))
	remote := "Away, back Monday; ask Sam, please.\nThanks"
	body, _ := json.Marshal(map[string]any{"enabled": true, "start": "2026-12-01T05:00:00Z", "end": "2026-12-05T05:00:00Z", "localMessage": "Away",
		"remote": "on", "remoteMessage": remote, "calendarAway": true, "calendarTentative": true})
	req := httptest.NewRequest(http.MethodPut, "/api/mail/out-of-office", bytes.NewReader(body))
	authRequestAs(f.srv, req, f.userID)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(got) != 1 || got[0]["comment_remote"] != remote {
		t.Fatalf("sync: %d %v", rec.Code, got)
	}

	f.srv.configPath = t.TempDir() + "/config.yaml"
	next := f.srv.cfg
	next.OutOfOfficeRemote = false
	cfg, _ := json.Marshal(next)
	req = httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(cfg))
	authRequestAs(f.srv, req, f.userID)
	rec = httptest.NewRecorder()
	f.srv.withAdmin(f.srv.handleConfig)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT: %d %s", rec.Code, rec.Body.String())
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 || time.Now().After(end) {
			break
		}
	}
	mu.Lock()
	if len(got) != 2 || got[1]["away"] != true || got[1]["comment_remote"] != nil {
		t.Fatalf("resync after revocation: %v", got)
	}
	mu.Unlock()

	// KyCalendar still holds the old comment (say the resync failed).
	escaped := `Away\, back Monday\; ask Sam\, please.\nThanks`
	got2 := mailedReply(t, f, "ev-ooo-1", "COMMENT:"+escaped[:20]+"\r\n "+escaped[20:]+"\r\nCOMMENT:See you\r\n")
	if strings.Contains(got2, "Away") || strings.Contains(got2, "back Monday") || !strings.Contains(got2, "COMMENT:See you") || !strings.Contains(got2, "PARTSTAT=ACCEPTED") {
		t.Fatalf("reply mailed:\n%s", got2)
	}
}

// A remote text changed, then cleared, while KyCalendar could not be reached
// is still refused: KyCalendar may hold any text sent since its last answer.
func TestOutOfOfficeRemoteCommentChangedWhileKyCalendarDown(t *testing.T) {
	f := newKyCalFixture(t)
	f.srv.cfgMu.Lock()
	f.srv.cfg.OutOfOfficeRemote = true
	f.srv.cfgMu.Unlock()
	status := http.StatusOK
	cal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	defer cal.Close()
	base, _ := url.Parse(cal.URL)
	f.srv.SetKyCalendar(kycalendar.New(base, kycalSecret, cal.Client().Transport))
	save := func(remote, text, want string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"enabled": true, "start": "2026-12-01T05:00:00Z", "end": "2026-12-05T05:00:00Z", "localMessage": "Away",
			"remote": remote, "remoteMessage": text, "calendarAway": true, "calendarTentative": true})
		req := httptest.NewRequest(http.MethodPut, "/api/mail/out-of-office", bytes.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		var v outOfOfficeView
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.CalendarSync != want {
			t.Fatalf("save %q: %d %s", text, rec.Code, rec.Body.String())
		}
	}
	save("on", "First text", "synced")
	status = http.StatusInternalServerError
	save("on", "Second text", "failed")
	save("off", "", "failed")
	for i, text := range []string{"First text", "Second text"} {
		if got := mailedReply(t, f, "ev-down-"+strconv.Itoa(i), "COMMENT:"+text+"\r\n"); strings.Contains(got, text) {
			t.Fatalf("stale %q mailed:\n%s", text, got)
		}
	}
	// Once KyCalendar takes a state without a remote comment, nothing is kept.
	status = http.StatusOK
	save("off", "", "synced")
	if o, err := f.srv.loadOutOfOffice(f.userID); err != nil || len(o.CalendarRemoteTexts) != 0 {
		t.Fatalf("kept %v %v", o.CalendarRemoteTexts, err)
	}
}

// mailedReply sends a REPLY carrying extra lines through the signed send route
// and returns the calendar part as mailed.
func mailedReply(t *testing.T, f *kycalFixture, eventID, extra string) string {
	t.Helper()
	if err := kycalendar.OpenInvitations(f.srv.userStateDir(f.userID)).Record("r1@x.test", "org@x.test"); err != nil {
		t.Fatal(err)
	}
	reply := sendBody(func(b map[string]any) {
		b["method"], b["ics"], b["recipients"] = "REPLY", strings.Replace(replyICS, "ORGANIZER:", extra+"ORGANIZER:", 1), []string{"org@x.test"}
	})
	if rec := signedSend(t, f.srv, kycalSecret, kycalendar.EventSend, eventID, reply); rec.Code != http.StatusAccepted {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body.String())
	}
	raw := <-f.delivered
	_, part, _ := strings.Cut(raw, "Content-Type: text/calendar; method=REPLY; charset=UTF-8\r\n\r\n")
	part, _, _ = strings.Cut(part, "\r\n--")
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(part, "\r\n", ""))
	if err != nil {
		t.Fatalf("calendar part: %v\n%s", err, raw)
	}
	return string(decoded)
}

func TestStripRemoteOOOComment(t *testing.T) {
	in := "BEGIN:VEVENT\r\nCOMMENT;LANGUAGE=\"en:gb\":Hi\\nthere\r\nComment:Hi\r\n \\nthere\r\nCOMMENT:Hi\r\nUID:x\r\nEND:VEVENT\r\n"
	want := "BEGIN:VEVENT\r\nCOMMENT:Hi\r\nUID:x\r\nEND:VEVENT\r\n"
	if got := string(stripRemoteOOOComment([]byte(in), []string{"", "Hi\r\nthere"})); got != want {
		t.Fatalf("got %q", got)
	}
}
