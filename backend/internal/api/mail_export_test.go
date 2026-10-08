//go:build linux

package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type writeCounter struct {
	*httptest.ResponseRecorder
	writes int
	broken bool
}

func (c *writeCounter) Write(p []byte) (int, error) {
	c.writes++
	if c.broken {
		return 0, io.ErrClosedPipe
	}
	return c.ResponseRecorder.Write(p)
}

func TestMailExport(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "export-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "export-two", 1, two))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-export"
	for _, id := range []string{one.ID, second.ID} {
		if _, err = srv.users.SetPassword(ctx, id, password, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := srv.users.Create(ctx, "export-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(ctx, "export-imap", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	type caller struct{ token, csrf string }
	session := func(id string) caller { tok, csrf := mintSessionForTest(srv, id); return caller{tok, csrf} }
	me, other, adm, imapUser := session(one.ID), session(second.ID), session(admin.ID), session(member.ID)
	srv.sessMu.Lock()
	srv.sessions["second-session"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "second-csrf"}
	srv.sessions["sso-session"] = Session{UserID: one.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true}
	srv.sessMu.Unlock()
	meAgain, meSSO := caller{"second-session", "second-csrf"}, caller{"sso-session", "sso-csrf"}

	serve := func(w http.ResponseWriter, method, path string, c caller, body string, header map[string]string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: c.token})
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		srv.routes().ServeHTTP(w, req)
	}
	call := func(method, path string, c caller, body string, header map[string]string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		serve(w, method, path, c, body, header)
		return w
	}
	start := func(c caller, fields string) *httptest.ResponseRecorder {
		return call("POST", "/api/export", c, `{`+fields+`"password":"`+password+`"}`, nil)
	}
	counted := -1
	grant := func(c caller, fields string) string {
		t.Helper()
		w := start(c, fields)
		var got struct {
			URL      string
			Expires  int `json:"expiresInSeconds"`
			Messages int
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !strings.HasPrefix(got.URL, "/api/export/") || got.Expires != 300 {
			t.Fatalf("start %s: %d %s", fields, w.Code, w.Body)
		}
		counted = got.Messages
		return got.URL
	}
	// A refused download sends the browser back to the export page.
	bounced := func(w *httptest.ResponseRecorder, code string) bool {
		return w.Code == http.StatusSeeOther && w.Header().Get("Location") == "/settings/security?tab=export&export="+code
	}
	adminCall := func(body string) *httptest.ResponseRecorder {
		return call("POST", "/api/admin/mailboxes", adm, body, nil)
	}
	w := adminCall(`{"user":"` + one.ID + `","address":"sales@example.test","password":"` + password + `"}`)
	var extra sso.NativeMailbox
	if json.Unmarshal(w.Body.Bytes(), &extra) != nil || w.Code != 200 {
		t.Fatal("create extra", w.Code, w.Body)
	}

	put := func(owner, mailboxID, folder string, raw []byte) {
		t.Helper()
		a, err := srv.ssoLifecycle.AdmitNativeMailbox(ctx, srv.stateDir, "https://idp.example", owner, mailboxID, srv.users)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if folder == "INBOX" {
			_, err = store.Import(ctx, mailbox.Receipt{Gateway: "export-test", Delivery: fmt.Sprint(time.Now().UnixNano()), Sender: "bounce@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw))
		} else {
			if err = store.CreateFolder(ctx, folder); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
				t.Fatal(err)
			}
			_, err = store.Append(ctx, folder, bytes.NewReader(raw), false)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	primaryRaw := []byte("From: sender@outside.test\r\nSubject: primary\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\nFrom x\r\n>From x\r\nbody\r\n")
	workRaw := []byte("From: sender@outside.test\r\nSubject: work\r\n\r\nwork only\r\n")
	extraRaw := []byte("From: sender@outside.test\r\nSubject: extra\r\n\r\nextra \x00 exact\r\n")
	put(one.ID, one.ID, "INBOX", primaryRaw)
	put(one.ID, one.ID, "Work", workRaw)
	put(one.ID, extra.ID, "INBOX", extraRaw)

	// Folder list for the selected mailbox; foreign mailboxes are one 404.
	var listed struct{ Folders []string }
	if w = call("GET", "/api/export/folders", me, "", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || !strings.Contains(fmt.Sprint(listed.Folders), "Work") {
		t.Fatal("folders", w.Code, w.Body)
	}
	if w = call("GET", "/api/export/folders", other, "", map[string]string{mailboxHeader: extra.ID}); w.Code != 404 {
		t.Fatal("foreign folders", w.Code, w.Body)
	}

	// Refusals before any grant exists.
	for _, c := range []struct {
		name string
		w    *httptest.ResponseRecorder
		want int
	}{
		{"no step-up", call("POST", "/api/export", me, `{"format":"mbox"}`, nil), 403},
		{"wrong password", call("POST", "/api/export", me, `{"format":"mbox","password":"wrong-password-here"}`, nil), 403},
		{"no csrf", call("POST", "/api/export", caller{me.token, ""}, `{"format":"mbox","password":"`+password+`"}`, nil), 403},
		{"bad format", start(me, `"format":"pst",`), 400},
		{"missing folder", start(me, `"format":"mbox","folder":"Nope",`), 404},
		{"unsafe folder", start(me, `"format":"mbox","folder":"a\\b",`), 400},
		{"foreign mailbox", start(other, `"format":"mbox","mailbox":"`+extra.ID+`",`), 404},
		{"path mailbox", start(me, `"format":"mbox","mailbox":"../users/`+second.ID+`",`), 404},
		{"imap account", start(imapUser, `"format":"mbox",`), 409},
		{"administrator", start(adm, `"format":"mbox",`), 409},
		{"administrator naming a user's mailbox", start(adm, `"format":"mbox","mailbox":"`+extra.ID+`",`), 404},
	} {
		if c.w.Code != c.want {
			t.Errorf("%s: got %d want %d: %s", c.name, c.w.Code, c.want, c.w.Body)
		}
	}
	if len(srv.exports) != 0 {
		t.Fatal("a refused request minted a grant")
	}
	if w = call("GET", "/api/export/folders", imapUser, "", nil); w.Code != 409 || !strings.Contains(w.Body.String(), "native mailboxes") {
		t.Fatal("imap folders", w.Code, w.Body)
	}

	// A KySignOn session must confirm this exact request.
	body := `{"format":"mbox"}`
	w = call("POST", "/api/export", meSSO, body, nil)
	var challenge struct{ Error, Challenge string }
	if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &challenge) != nil || challenge.Error != "sso_step_up_required" {
		t.Fatal("KySignOn session skipped step-up", w.Code, w.Body)
	}
	srv.stepUpMu.Lock()
	c := srv.stepUps[challenge.Challenge]
	c.verified = true
	srv.stepUps[challenge.Challenge] = c
	srv.stepUpMu.Unlock()
	if w = call("POST", "/api/export", meSSO, `{"format":"eml-zip"}`, map[string]string{stepUpHeader: challenge.Challenge}); w.Code != 403 {
		t.Fatal("grant spent on a different request", w.Code, w.Body)
	}
	if w = call("POST", "/api/export", meSSO, body, map[string]string{stepUpHeader: challenge.Challenge}); w.Code != 200 {
		t.Fatal("confirmed KySignOn export", w.Code, w.Body)
	}

	// mbox of the whole primary: bound to user and session, single use.
	url := grant(me, `"format":"mbox",`)
	if counted != 2 {
		t.Fatal("message count", counted)
	}
	if len(srv.exports) != 1 {
		t.Fatal("a new grant did not replace the user's previous one", len(srv.exports))
	}
	for _, c := range []caller{meAgain, other, meSSO} {
		if w = call("GET", url, c, "", nil); !bounced(w, "expired") {
			t.Fatal("grant crossed sessions or users", c.token, w.Code, w.Header())
		}
	}
	if w = call("GET", "/api/export/unknown", me, "", nil); !bounced(w, "expired") {
		t.Fatal("unknown token", w.Code)
	}
	// HEAD neither streams nor spends the grant.
	if w = call("HEAD", url, me, "", nil); w.Code != 405 || len(srv.exports) != 1 {
		t.Fatal("HEAD", w.Code, len(srv.exports))
	}
	// A POST arriving over HTTP/1.0 cannot export mbox.
	req := httptest.NewRequest("POST", "/api/export", strings.NewReader(`{"format":"mbox","password":"`+password+`"}`))
	req.Proto, req.ProtoMinor = "HTTP/1.0", 0
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: me.token})
	req.Header.Set("X-CSRF-Token", me.csrf)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "proxy_http_version 1.1") || len(srv.exports) != 1 {
		t.Fatal("HTTP/1.0 mbox start", w.Code, w.Body)
	}
	w = call("GET", url, me, "", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/mbox" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="kypost-mail-`) {
		t.Fatal("mbox download", w.Code, w.Header(), w.Body)
	}
	got := w.Body.String()
	if strings.Count(got, "\nFrom ")+1 != 2 || !strings.HasPrefix(got, "From bounce@outside.test Tue Sep  1 10:00:00 2026\n") ||
		!strings.Contains(got, "\r\n\r\n>From x\r\n>>From x\r\nbody\r\n\n") || !strings.Contains(got, "From MAILER-DAEMON ") || strings.Contains(got, "extra") {
		t.Fatalf("mbox content %q", got)
	}
	if w = call("GET", url, me, "", nil); !bounced(w, "expired") {
		t.Fatal("grant reused", w.Code)
	}

	// One folder, then the extra mailbox as a zip of exact bytes.
	if w = start(me, `"format":"mbox","folder":"work",`); w.Code != 404 {
		t.Fatal("folder names are exact apart from INBOX", w.Code)
	}
	if w = call("GET", grant(me, `"format":"mbox","folder":"Work",`), me, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "work only") || strings.Contains(w.Body.String(), "primary") {
		t.Fatal("folder export", w.Code, w.Body)
	}
	w = call("GET", grant(me, `"format":"eml-zip","mailbox":"`+extra.ID+`",`), me, "", nil)
	if counted != 1 {
		t.Fatal("extra count", counted)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || err != nil || len(zr.File) != 1 || !strings.HasPrefix(zr.File[0].Name, "INBOX/") {
		t.Fatal("zip download", w.Code, err)
	}
	rc, _ := zr.File[0].Open()
	if eml, _ := io.ReadAll(rc); !bytes.Equal(eml, extraRaw) {
		t.Fatalf("eml bytes %q", eml)
	}

	// Expiry.
	url = grant(me, `"format":"mbox",`)
	token := strings.TrimPrefix(url, "/api/export/")
	srv.exportMu.Lock()
	g := srv.exports[token]
	g.expires = time.Now().Add(-time.Second)
	srv.exports[token] = g
	srv.exportMu.Unlock()
	if w = call("GET", url, me, "", nil); !bounced(w, "expired") || len(srv.exports) != 0 {
		t.Fatal("expired grant", w.Code, len(srv.exports))
	}

	// Concurrency: one per user, maxExports overall; a busy refusal keeps the grant.
	url = grant(me, `"format":"mbox",`)
	for _, busy := range [][]string{{one.ID}, {"someone", "someone-else"}} {
		srv.exportMu.Lock()
		srv.exporting = map[string]bool{}
		for _, id := range busy {
			srv.exporting[id] = true
		}
		srv.exportMu.Unlock()
		if w = call("GET", url, me, "", nil); !bounced(w, "busy&retry="+strings.TrimPrefix(url, "/api/export/")) || len(srv.exports) != 1 {
			t.Fatal("busy export", busy, w.Code)
		}
	}
	srv.exportMu.Lock()
	srv.exporting = map[string]bool{}
	srv.exportMu.Unlock()
	if w = call("GET", url, me, "", nil); w.Code != 200 || len(srv.exporting) != 0 {
		t.Fatal("export after the slot freed", w.Code, len(srv.exporting))
	}

	// Streaming: many messages reach the writer in many bounded writes.
	big := bytes.Repeat([]byte("0123456789abcdef"), 256)
	for i := range 120 {
		put(one.ID, extra.ID, "INBOX", append([]byte(fmt.Sprintf("Subject: %d\r\n\r\n", i)), big...))
	}
	counter := &writeCounter{ResponseRecorder: httptest.NewRecorder()}
	serve(counter, "GET", grant(me, `"format":"mbox","mailbox":"`+extra.ID+`",`), me, "", nil)
	if counter.Code != 200 || strings.Count(counter.Body.String(), "\nFrom MAILER-DAEMON")+strings.Count(counter.Body.String(), "\nFrom bounce") < 120 || counter.writes < 6 {
		t.Fatal("streaming", counter.Code, counter.writes, counter.Body.Len())
	}

	// A failed write aborts the connection rather than ending a truncated
	// file cleanly, and frees the slot.
	broken := &writeCounter{ResponseRecorder: httptest.NewRecorder(), broken: true}
	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Fatal("failed export did not abort", p)
			}
		}()
		serve(broken, "GET", grant(me, `"format":"eml-zip","mailbox":"`+extra.ID+`",`), me, "", nil)
	}()
	if broken.writes != 1 || len(srv.exporting) != 0 || len(srv.exports) != 0 {
		t.Fatal("failed export state", broken.writes, len(srv.exporting), len(srv.exports))
	}

	// Over real HTTP/1.1 an aborted stream is a truncated chunked body, not a
	// clean end. The mailbox is disabled after the first page reaches the
	// wire; the per-page admission check stops the export.
	for i := range 100 {
		put(one.ID, extra.ID, "INBOX", append([]byte(fmt.Sprintf("Subject: more %d\r\n\r\n", i)), big...))
	}
	toggle := func(action string) int {
		return call("POST", "/api/admin/mailboxes/"+extra.ID+"/"+action, adm, `{"password":"`+password+`"}`, nil).Code
	}
	disabled := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.routes().ServeHTTP(&onFirstWrite{ResponseWriter: w, fn: func() { disabled = toggle("disable") }}, r)
	}))
	defer ts.Close()
	get := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: me.token})
		client := ts.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get(grant(me, `"format":"mbox","mailbox":"`+extra.ID+`",`))
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || fmt.Sprint(resp.TransferEncoding) != "[chunked]" || !errors.Is(err, io.ErrUnexpectedEOF) || disabled != 200 {
		t.Fatal("aborted export looked complete", resp.StatusCode, resp.TransferEncoding, err, disabled)
	}
	if code := toggle("enable"); code != 200 {
		t.Fatal("re-enable", code)
	}

	// mbox over HTTP/1.0 is refused at download too, keeping the grant.
	url = grant(me, `"format":"mbox",`)
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: kypost.test\r\nCookie: kypost_session=%s\r\n\r\n", url, me.token)
	resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/security?tab=export&export=proxy" || len(srv.exports) != 1 {
		t.Fatal("HTTP/1.0 mbox download", err, resp, len(srv.exports))
	}
	if resp = get(url); resp.StatusCode != 200 {
		t.Fatal("grant kept after the HTTP/1.0 refusal", resp.StatusCode)
	}
	resp.Body.Close()
}

type onFirstWrite struct {
	http.ResponseWriter
	fn   func()
	done bool
}

func (o *onFirstWrite) Write(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.fn()
	}
	return o.ResponseWriter.Write(p)
}

func (o *onFirstWrite) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func TestExportWriteWindow(t *testing.T) {
	for size, want := range map[int]time.Duration{0: 30 * time.Second, 64 << 10: 31 * time.Second, 25 << 20: 430 * time.Second} {
		if got := exportWriteWindow(size); got != want {
			t.Errorf("exportWriteWindow(%d) = %s, want %s", size, got, want)
		}
	}
}

// A paired device cannot start an export, a promoted KyIdentity administrator
// gets the administrator 403, and a migrated legacyMixedUse administrator
// keeps exporting their own primary mailbox.
func TestMailExportIdentities(t *testing.T) {
	ctx := context.Background()
	const password = "long-password-for-export"
	start := func(s *Server, userID string, device ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/export", strings.NewReader(`{"format":"mbox","password":"`+password+`"}`))
		r.Header.Set("Content-Type", "application/json")
		if device != nil {
			setDeviceHeaders(r, device[0], device[1])
		} else {
			authRequestAs(s, r, userID)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}

	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "everyday-create", 1, adminRuntimeUser(false)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.SetPassword(ctx, u.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	deviceID, secret := pairNativeDevice(t, s, u.ID, "export-device")
	if w := start(s, u.ID, deviceID, secret); w.Code != 403 || !strings.Contains(w.Body.String(), "browser session") || len(s.exports) != 0 {
		t.Fatal("device started an export", w.Code, w.Body)
	}
	if w := start(s, u.ID); w.Code != 200 {
		t.Fatal("everyday export", w.Code, w.Body)
	}
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "everyday-promote", 2, adminRuntimeUser(true)))
	assertAdministratorRefusal(t, "promoted export", start(s, u.ID))
	assertAdministratorRefusal(t, "promoted export folders", requestAs(s, u.ID, "GET", "/api/export/folders"))

	legacy := newNativeRuntimeServer(t)
	legacyMixedUseAdmin(t, legacy, runtimeDirectoryUser(true))
	lu, err := legacy.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || lu.Role != users.RoleAdmin {
		t.Fatal("legacy administrator", lu.Role, err)
	}
	if _, err = legacy.users.SetPassword(ctx, lu.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	w := start(legacy, lu.ID)
	var got struct{ URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatal("legacy administrator export", w.Code, w.Body)
	}
	if w = requestAs(legacy, lu.ID, "GET", got.URL); w.Code != 200 || w.Header().Get("Content-Type") != "application/mbox" {
		t.Fatal("legacy administrator download", w.Code, w.Header())
	}
}
