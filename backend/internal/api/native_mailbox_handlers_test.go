//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailboxesAdminAPIAndSelection(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "mailboxes-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "mailboxes-two", 1, two))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-mailboxes"
	admin, err := srv.users.Create(ctx, "mailboxes-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	mtoken, mcsrf := mintSessionForTest(srv, one.ID)
	call := func(method, path, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		}
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	confirm := func(fields string) string { return `{` + fields + `"password":"` + password + `"}` }

	// Same gates as the address routes.
	for _, route := range []string{"/api/admin/mailboxes", "/api/admin/mailboxes/mbx-x/disable", "/api/admin/mailboxes/mbx-x/enable"} {
		for _, c := range []struct {
			token, csrf string
			want        int
		}{{"", "", 401}, {mtoken, mcsrf, 403}, {token, "", 403}, {token, csrf, 401}} {
			if w := call("POST", route, c.token, c.csrf, "{}"); w.Code != c.want {
				t.Fatalf("auth %s: got %d want %d: %s", route, w.Code, c.want, w.Body)
			}
		}
	}
	if w := call("GET", "/api/admin/mailboxes", mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member listed mailboxes", w.Code)
	}
	create := func(user, address string) *httptest.ResponseRecorder {
		return call("POST", "/api/admin/mailboxes", token, csrf, confirm(`"user":"`+user+`","address":"`+address+`",`))
	}
	for _, c := range []struct {
		user, address string
		want          int
	}{
		{one.ID, "not an address", 400},
		{"nobody", "sales@example.test", 404},
		{admin.ID, "sales@example.test", 409},
		{one.ID, "two@example.test", 409},
		{one.ID, "sales@unconfigured.test", 409},
	} {
		if w := create(c.user, c.address); w.Code != c.want {
			t.Fatalf("create %s for %s: got %d want %d: %s", c.address, c.user, w.Code, c.want, w.Body)
		}
	}
	w := create(one.ID, "sales@example.test")
	var extra sso.NativeMailbox
	if err = json.Unmarshal(w.Body.Bytes(), &extra); w.Code != 200 || err != nil || extra.User != one.ID || extra.Kind != "extra" || extra.State != "active" {
		t.Fatal("create", w.Code, w.Body)
	}
	var listed struct{ Mailboxes []sso.NativeMailbox }
	if w = call("GET", "/api/admin/mailboxes?user="+one.ID, token, csrf, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Mailboxes) != 2 {
		t.Fatal("list", w.Code, w.Body)
	}

	// One message in each mailbox.
	put := func(mailboxID, subject string) {
		t.Helper()
		a, err := srv.ssoLifecycle.AdmitNativeMailbox(ctx, srv.stateDir, "https://idp.example", one.ID, mailboxID, srv.users)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		raw := mailmsg.Message{From: "sender@outside.test", To: []string{a.Address}, Subject: subject, Body: subject + " body"}.Build()
		if _, err := store.Import(ctx, mailbox.Receipt{Gateway: "qualified-test", Delivery: subject, Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	put(one.ID, "primary-only-subject")
	put(extra.ID, "extra-only-subject")
	as := func(userID, method, path, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		authRequestAs(srv, req, userID)
		if header != "" {
			req.Header.Set(mailboxHeader, header)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	sees := func(w *httptest.ResponseRecorder, want, hidden string) {
		t.Helper()
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), hidden) {
			t.Fatalf("want %s without %s: %d %s", want, hidden, w.Code, w.Body)
		}
	}
	// Old clients (no header) and the primary's own ID see only the primary.
	sees(as(one.ID, "GET", "/api/inbox?limit=10", ""), "primary-only-subject", "extra-only-subject")
	sees(as(one.ID, "GET", "/api/inbox?limit=10", one.ID), "primary-only-subject", "extra-only-subject")
	sees(as(one.ID, "GET", "/api/inbox?limit=10", extra.ID), "extra-only-subject", "primary-only-subject")
	sees(as(one.ID, "GET", "/api/mail/search?q=only-subject", ""), "primary-only-subject", "extra-only-subject")
	sees(as(one.ID, "GET", "/api/mail/search?q=only-subject", extra.ID), "extra-only-subject", "primary-only-subject")
	srv.userMu.Lock()
	_, primaryClient := srv.userMail[one.ID]
	_, extraClient := srv.userMail[extra.ID]
	_, extraCache := srv.userMailCache[extra.ID]
	_, extraState := srv.userStores[extra.ID]
	srv.userMu.Unlock()
	if !primaryClient || !extraClient || !extraCache || !extraState {
		t.Fatal("mail caches are not keyed by mailbox ID")
	}

	// Foreign, unknown and disabled mailboxes answer one 404 and open nothing.
	refused := func(userID, header string) {
		t.Helper()
		w := as(userID, "GET", "/api/inbox?limit=10", header)
		if w.Code != 404 || strings.TrimSpace(w.Body.String()) != `{"error":"mailbox not found"}` {
			t.Fatalf("mailbox %s for %s: %d %s", header, userID, w.Code, w.Body)
		}
	}
	srv.userMu.Lock()
	delete(srv.userMail, extra.ID)
	delete(srv.userMailCache, extra.ID)
	delete(srv.userStores, extra.ID)
	srv.userMu.Unlock()
	refused(second.ID, extra.ID)
	refused(one.ID, "mbx-unknown")
	refused(one.ID, "../users/"+second.ID)
	refused(one.ID, second.ID)
	if _, err := os.Lstat(filepath.Join(srv.stateDir, "mailboxes", "mbx-unknown")); !os.IsNotExist(err) {
		t.Fatal("unknown mailbox created storage", err)
	}
	srv.userMu.Lock()
	_, extraClient = srv.userMail[extra.ID]
	_, extraCache = srv.userMailCache[extra.ID]
	_, extraState = srv.userStores[extra.ID]
	_, secondClient := srv.userMail[second.ID]
	clients := len(srv.userMail)
	srv.userMu.Unlock()
	if extraClient || extraCache || extraState || secondClient || clients != 1 {
		t.Fatal("a refused request opened a mailbox")
	}
	// Per-user endpoints ignore the header.
	if w := as(one.ID, "GET", "/api/notifications/native/devices", "mbx-unknown"); w.Code != 200 {
		t.Fatal("per-user endpoint honoured the header", w.Code, w.Body)
	}
	if w := as(one.ID, "GET", "/api/contacts", "mbx-unknown"); w.Code != 200 {
		t.Fatal("contacts honoured the header", w.Code, w.Body)
	}
	var mine struct {
		Mailboxes []struct {
			ID, Kind  string
			Addresses []struct{ Address, Kind string }
		}
	}
	w = as(one.ID, "GET", "/api/mailboxes", "mbx-unknown")
	if err = json.Unmarshal(w.Body.Bytes(), &mine); w.Code != 200 || err != nil || len(mine.Mailboxes) != 2 || mine.Mailboxes[0].ID != one.ID || mine.Mailboxes[1].ID != extra.ID || mine.Mailboxes[1].Addresses[0].Address != "sales@example.test" {
		t.Fatal("caller mailboxes", w.Code, w.Body)
	}
	if w = as(second.ID, "GET", "/api/mailboxes", ""); !strings.Contains(w.Body.String(), second.ID) || strings.Contains(w.Body.String(), extra.ID) {
		t.Fatal("another user's mailboxes listed", w.Body)
	}

	// From is an active address of the sending mailbox; with no relay an
	// accepted From ends in the outbox's 503.
	send := func(header, from string) int {
		req := httptest.NewRequest("POST", "/api/mail/send", strings.NewReader(`{"to":"visible@outside.test","subject":"s","body":"b","from":"`+from+`"}`))
		req.Header.Set("Content-Type", "application/json")
		authRequestAs(srv, req, one.ID)
		if header != "" {
			req.Header.Set(mailboxHeader, header)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w.Code
	}
	for _, c := range []struct {
		header, from string
		want         int
	}{{"", "sales@example.test", 403}, {extra.ID, "one@example.test", 403}, {extra.ID, "sales@example.test", 503}, {extra.ID, "", 503}, {"", "", 503}} {
		if got := send(c.header, c.from); got != c.want {
			t.Fatalf("send from %q in %q: got %d want %d", c.from, c.header, got, c.want)
		}
	}

	// With a relay that refuses connections the job stays in the sending
	// mailbox's own outbox, visible only with that mailbox selected.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	if _, err = mailmsg.SaveDomainRelay(ctx, filepath.Join(srv.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: port, Username: "relay-login", Password: "relay-secret"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/mail/send", strings.NewReader(`{"to":"visible@outside.test","subject":"s","body":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	authRequestAs(srv, req, one.ID)
	req.Header.Set(mailboxHeader, extra.ID)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	var queued struct{ OutboxID string }
	if err = json.Unmarshal(w.Body.Bytes(), &queued); w.Code != 503 || err != nil || queued.OutboxID == "" {
		t.Fatal("unconfirmed send from the extra mailbox", w.Code, w.Body)
	}
	if w = as(one.ID, "GET", "/api/mail/outbox/"+queued.OutboxID, extra.ID); w.Code != 200 {
		t.Fatal("extra mailbox outbox", w.Code, w.Body)
	}
	if w = as(one.ID, "GET", "/api/mail/outbox/"+queued.OutboxID, ""); w.Code == 200 {
		t.Fatal("extra mailbox job found in the primary outbox", w.Body)
	}

	// Disable: omitted from the list, same 404, mail retained; enable restores.
	if w = call("POST", "/api/admin/mailboxes/"+extra.ID+"/disable", token, csrf, confirm("")); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"disabled"`) {
		t.Fatal("disable", w.Code, w.Body)
	}
	if w = call("POST", "/api/admin/mailboxes/"+extra.ID+"/disable", token, csrf, confirm("")); w.Code != 409 {
		t.Fatal("disabled twice", w.Code)
	}
	if w = call("POST", "/api/admin/mailboxes/"+one.ID+"/disable", token, csrf, confirm("")); w.Code != 404 {
		t.Fatal("primary disabled", w.Code)
	}
	refused(one.ID, extra.ID)
	if w = as(one.ID, "GET", "/api/mailboxes", ""); w.Code != 200 || strings.Contains(w.Body.String(), extra.ID) {
		t.Fatal("disabled mailbox listed", w.Body)
	}
	if w = call("POST", "/api/admin/mailboxes/"+extra.ID+"/enable", token, csrf, confirm("")); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatal("enable", w.Code, w.Body)
	}
	sees(as(one.ID, "GET", "/api/inbox?limit=10", extra.ID), "extra-only-subject", "primary-only-subject")
}
