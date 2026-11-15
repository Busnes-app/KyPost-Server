package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
