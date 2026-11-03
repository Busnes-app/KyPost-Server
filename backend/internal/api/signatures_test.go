package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

func signaturesRequest(t *testing.T, srv *Server, method, body string, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/api/mail/signatures", nil)
	} else {
		req = httptest.NewRequest(method, "/api/mail/signatures", strings.NewReader(body))
	}
	auth(req)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// A new account has no signatures, with replies and forwards on, and what one
// client saves is what every client reads back.
func TestSignaturesRoundTrip(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	get := func() config.UserSignatureSettings {
		rec := httptest.NewRecorder()
		srv.handleSignatures(rec, authedRequestForTest(httptest.NewRequest(http.MethodGet, "/api/mail/signatures", nil), u))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		var got config.UserSignatureSettings
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return got
	}
	if got := get(); len(got.Signatures) != 0 || got.DefaultID != "" || !got.OnReply || !got.OnForward || got.ByAddress == nil {
		t.Fatalf("defaults = %+v, want none, replies and forwards on", got)
	}

	body := `{"signatures":[{"id":"work","name":"  Work ","text":"Ada Lovelace\r\n<b>Analyst</b>"},{"id":"short","name":"Short","text":"A."}],` +
		`"defaultId":"work","byAddress":{"Ada@Example.org":"short","alias@example.org":""},"onReply":true,"onForward":false}`
	rec := httptest.NewRecorder()
	srv.handleSignatures(rec, authedRequestForTest(httptest.NewRequest(http.MethodPut, "/api/mail/signatures", strings.NewReader(body)), u))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	got := get()
	if len(got.Signatures) != 2 || got.Signatures[0].Name != "Work" || got.Signatures[0].Text != "Ada Lovelace\n<b>Analyst</b>" {
		t.Fatalf("signatures = %+v, want trimmed name, \\n line endings, markup kept as text", got.Signatures)
	}
	if got.DefaultID != "work" || got.ByAddress["ada@example.org"] != "short" || got.ByAddress["alias@example.org"] != "" || len(got.ByAddress) != 2 {
		t.Fatalf("assignment = %q %v", got.DefaultID, got.ByAddress)
	}
	if !got.OnReply || got.OnForward {
		t.Fatalf("onReply/onForward = %v/%v, want true/false", got.OnReply, got.OnForward)
	}

	// The other settings in the same file survive a signature save.
	settings, err := config.LoadUserSettings(srv.userSettingsPath(u.ID))
	if err != nil || settings.Notifications.Mode != "none" {
		t.Fatalf("notification settings after signature save: %+v %v", settings.Notifications, err)
	}
}

func TestSignaturesAreValidated(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	sig := func(id, name, text string) string {
		b, _ := json.Marshal(config.Signature{ID: id, Name: name, Text: text})
		return string(b)
	}
	many := make([]string, config.MaxSignatures+1)
	for i := range many {
		many[i] = sig("s"+strings.Repeat("x", i+1), "n", "t")
	}
	for name, body := range map[string]string{
		"html field":        `{"signatures":[{"id":"a","name":"A","text":"x","html":"<img src=x onerror=alert(1)>"}]}`,
		"unknown top field": `{"signatures":[],"html":true}`,
		"bad id":            `{"signatures":[` + sig("../x", "A", "x") + `]}`,
		"duplicate id":      `{"signatures":[` + sig("a", "A", "x") + `,` + sig("a", "B", "y") + `]}`,
		"empty name":        `{"signatures":[` + sig("a", "  ", "x") + `]}`,
		"long name":         `{"signatures":[` + sig("a", strings.Repeat("n", config.MaxSignatureNameRunes+1), "x") + `]}`,
		"long text":         `{"signatures":[` + sig("a", "A", strings.Repeat("é", config.MaxSignatureTextRunes+1)) + `]}`,
		"control in text":   `{"signatures":[` + sig("a", "A", "x\u0007y") + `]}`,
		"bidi in text":      `{"signatures":[` + sig("a", "A", "x\u202ey") + `]}`,
		"control in name":   `{"signatures":[` + sig("a", "A\nB", "x") + `]}`,
		"too many":          `{"signatures":[` + strings.Join(many, ",") + `]}`,
		"missing default":   `{"signatures":[` + sig("a", "A", "x") + `],"defaultId":"b"}`,
		"missing for addr":  `{"signatures":[` + sig("a", "A", "x") + `],"byAddress":{"me@example.org":"b"}}`,
		"not an address":    `{"signatures":[],"byAddress":{"Me <me@example.org>":""}}`,
		"address twice":     `{"signatures":[],"byAddress":{"me@example.org":"","ME@example.org":""}}`,
	} {
		rec := httptest.NewRecorder()
		srv.handleSignatures(rec, authedRequestForTest(httptest.NewRequest(http.MethodPut, "/api/mail/signatures", strings.NewReader(body)), u))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	settings, err := config.LoadUserSettings(srv.userSettingsPath(u.ID))
	if err != nil || len(settings.Signatures.Signatures) != 0 {
		t.Fatalf("a refused PUT stored something: %+v %v", settings.Signatures, err)
	}
	// Tabs and newlines are text, not control characters.
	rec := httptest.NewRecorder()
	srv.handleSignatures(rec, authedRequestForTest(httptest.NewRequest(http.MethodPut, "/api/mail/signatures",
		strings.NewReader(`{"signatures":[`+sig("a", "A", "line\tone\nline two")+`]}`)), u))
	if rec.Code != http.StatusOK {
		t.Fatalf("tab/newline text: %d %s", rec.Code, rec.Body.String())
	}
}

// A paired device reads the same signatures the web app saved, and may not
// write them: settings writes stay on the web session (withAuth), as for rules.
func TestSignaturesDeviceReadsButCannotWrite(t *testing.T) {
	srv, userID, _, authDevice := newPairedDeviceForTest(t)
	if err := config.UpdateUserSettings(srv.userSettingsPath(userID), func(s *config.UserSettings) error {
		s.Signatures.Signatures = []config.Signature{{ID: "a", Name: "A", Text: "Sent from my desk"}}
		s.Signatures.DefaultID = "a"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := signaturesRequest(t, srv, http.MethodGet, "", authDevice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sent from my desk") {
		t.Fatalf("device GET: %d %s", rec.Code, rec.Body.String())
	}
	rec = signaturesRequest(t, srv, http.MethodPut, `{"signatures":[]}`, authDevice)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("device PUT: %d %s, want 401", rec.Code, rec.Body.String())
	}
	rec = signaturesRequest(t, srv, http.MethodGet, "", func(*http.Request) {})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET: %d, want 401", rec.Code)
	}
}
