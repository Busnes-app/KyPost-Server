package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func oooPut(t *testing.T, srv *Server, u users.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleOutOfOffice(rec, authedRequestForTest(httptest.NewRequest(http.MethodPut, "/api/mail/out-of-office", strings.NewReader(body)), u))
	return rec
}

func TestOutOfOfficeRoundTrip(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	rec := httptest.NewRecorder()
	srv.handleOutOfOffice(rec, authedRequestForTest(httptest.NewRequest(http.MethodGet, "/api/mail/out-of-office", nil), u))
	var got outOfOfficeView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.Enabled || got.Remote != config.RemoteOff || got.RemoteAllowed {
		t.Fatalf("default: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("GET is cacheable")
	}
	body := `{"enabled":true,"start":"2026-12-01T09:00:00+01:00","end":"2026-12-05T17:00:00+01:00","timeZone":"Europe/Paris","localMessage":"Away\r\nback Monday","remote":"contacts","remoteMessage":""}`
	rec = oooPut(t, srv, u, body)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Start != "2026-12-01T08:00:00Z" || got.LocalMessage != "Away\nback Monday" || got.Remote != config.RemoteContacts || strings.Contains(rec.Body.String(), "period") {
		t.Fatalf("stored %s", rec.Body.String())
	}
	first, _ := srv.loadOutOfOffice(u.ID)
	if first.Period == "" {
		t.Fatal("no period")
	}
	// Same dates keep the period; new dates renew it.
	oooPut(t, srv, u, strings.Replace(body, "back Monday", "back Tuesday", 1))
	if o, _ := srv.loadOutOfOffice(u.ID); o.Period != first.Period {
		t.Error("text change renewed the period")
	}
	oooPut(t, srv, u, strings.Replace(body, "12-05", "12-06", 1))
	if o, _ := srv.loadOutOfOffice(u.ID); o.Period == first.Period {
		t.Error("date change kept the period")
	}
}

func TestOutOfOfficeIsValidated(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	srv.cfg.OutOfOfficeRemote = true
	for name, body := range map[string]string{
		"unknown field":       `{"enabled":false,"period":"mine"}`,
		"bad remote":          `{"remote":"everyone"}`,
		"bad start":           `{"start":"tomorrow"}`,
		"end before start":    `{"start":"2026-12-05T00:00:00Z","end":"2026-12-01T00:00:00Z"}`,
		"bad zone":            `{"timeZone":"Mars/Olympus"}`,
		"local zone":          `{"timeZone":"Local"}`,
		"long local":          `{"localMessage":"` + strings.Repeat("é", config.MaxOutOfOfficeRunes+1) + `"}`,
		"control in remote":   `{"remoteMessage":"a\u0007b"}`,
		"bidi in local":       `{"localMessage":"a‮b"}`,
		"on without text":     `{"enabled":true}`,
		"remote without text": `{"enabled":true,"localMessage":"x","remote":"on"}`,
		"not json":            `{`,
		"oversize body":       `{"localMessage":"` + strings.Repeat("a", 40<<10) + `"}`,
	} {
		if rec := oooPut(t, srv, u, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	if o, _ := srv.loadOutOfOffice(u.ID); o != (config.OutOfOffice{}) {
		t.Fatalf("a refused PUT stored %+v", o)
	}
	// With outside replies off, the remote text is not needed to turn on.
	srv.cfg.OutOfOfficeRemote = false
	if rec := oooPut(t, srv, u, `{"enabled":true,"localMessage":"x","remote":"on"}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func oooRequest(srv *Server, method, body string, auth func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/mail/out-of-office", strings.NewReader(body))
	auth(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// A paired device reads the setting and may not write it; nobody else reads it.
func TestOutOfOfficeDeviceReadsButCannotWrite(t *testing.T) {
	srv, _, _, authDevice := newPairedDeviceForTest(t)
	if rec := oooRequest(srv, http.MethodGet, "", authDevice); rec.Code != 200 {
		t.Fatalf("device GET: %d", rec.Code)
	}
	if rec := oooRequest(srv, http.MethodPut, `{"enabled":false}`, authDevice); rec.Code != http.StatusUnauthorized {
		t.Fatalf("device PUT: %d, want 401", rec.Code)
	}
	if rec := oooRequest(srv, http.MethodGet, "", func(*http.Request) {}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET: %d, want 401", rec.Code)
	}
}

// A dedicated administrator identity has no mailbox to reply from.
func TestOutOfOfficeRefusesNativeAdministrator(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "admin-create", 1, adminRuntimeUser(true)))
	u := assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
	assertAdministratorRefusal(t, "GET", requestAs(s, u.ID, "GET", "/api/mail/out-of-office"))
	assertAdministratorRefusal(t, "PUT", requestBodyAs(s, u.ID, "PUT", "/api/mail/out-of-office", `{"enabled":false}`))
}
