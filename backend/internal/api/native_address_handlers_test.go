//go:build linux

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailAddressesAdminAPI(t *testing.T) {
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "address-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "address-two", 1, two))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-addresses"
	admin, err := srv.users.Create(context.Background(), "addresses-admin", password, users.RoleAdmin)
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
	mutations := []struct{ method, path string }{{"POST", "/api/admin/mail-addresses"}, {"DELETE", "/api/admin/mail-addresses/sales@example.test"}, {"POST", "/api/admin/mail-addresses/sales@example.test/reassign"}}
	for _, route := range mutations {
		for _, c := range []struct {
			token, csrf string
			want        int
		}{{"", "", 401}, {mtoken, mcsrf, 403}, {token, "", 403}, {token, csrf, 401}} {
			if w := call(route.method, route.path, c.token, c.csrf, "{}"); w.Code != c.want {
				t.Fatalf("auth %s %s: got %d want %d: %s", route.method, route.path, w.Code, c.want, w.Body)
			}
		}
	}
	if w := call("GET", "/api/admin/mail-addresses", mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member listed addresses", w.Code)
	}
	// A KySignOn session must re-prove presence for this exact request.
	srv.sessMu.Lock()
	srv.sessions["sso-admin"] = Session{UserID: admin.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "sso-csrf", SSOKySignOn: true, SSOAppAdmin: true}
	srv.sessMu.Unlock()
	if w := call("POST", "/api/admin/mail-addresses", "sso-admin", "sso-csrf", `{"mailbox":"`+one.ID+`","address":"sales@example.test"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "sso_step_up_required") {
		t.Fatal("KySignOn session skipped step-up", w.Code, w.Body)
	}
	add := func(mailbox, address string) *httptest.ResponseRecorder {
		return call("POST", "/api/admin/mail-addresses", token, csrf, confirm(`"mailbox":"`+mailbox+`","address":"`+address+`",`))
	}
	for _, c := range []struct {
		mailbox, address string
		want             int
	}{
		{one.ID, "not an address", 400},
		{"missing-mailbox", "sales@example.test", 404},
		{one.ID, "two@example.test", 409},
		{one.ID, "sales@unconfigured.test", 409},
	} {
		if w := add(c.mailbox, c.address); w.Code != c.want {
			t.Fatalf("add %s: got %d want %d: %s", c.address, w.Code, c.want, w.Body)
		}
	}
	w := add(one.ID, "sales@example.test")
	var got sso.NativeAddress
	if err = json.Unmarshal(w.Body.Bytes(), &got); w.Code != 200 || err != nil || got != (sso.NativeAddress{Address: "sales@example.test", Mailbox: one.ID, Kind: "alias", State: "active", Generation: 1}) {
		t.Fatal("add", w.Code, w.Body)
	}
	if w = add(second.ID, "sales@example.test"); w.Code != 409 {
		t.Fatal("taken alias", w.Code)
	}
	w = call("GET", "/api/admin/mail-addresses?user="+one.ID, token, csrf, "")
	var listed struct{ Mailboxes []sso.NativeMailbox }
	if err = json.Unmarshal(w.Body.Bytes(), &listed); w.Code != 200 || err != nil || len(listed.Mailboxes) != 1 || len(listed.Mailboxes[0].Addresses) != 2 || listed.Mailboxes[0].User != one.ID {
		t.Fatal("filtered list", w.Code, w.Body)
	}
	if w = call("GET", "/api/admin/mail-addresses?user=nobody", token, csrf, ""); w.Code != 404 {
		t.Fatal("unknown user filter", w.Code)
	}
	if w = call("GET", "/api/admin/mail-addresses", token, csrf, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Mailboxes) != 2 {
		t.Fatal("full list", w.Code, w.Body)
	}
	if w = call("DELETE", "/api/admin/mail-addresses/one@example.test", token, csrf, confirm("")); w.Code != 409 {
		t.Fatal("primary released", w.Code)
	}
	if w = call("POST", "/api/admin/mail-addresses/sales@example.test/reassign", token, csrf, confirm(`"mailbox":"`+second.ID+`",`)); w.Code != 409 {
		t.Fatal("active alias reassigned", w.Code)
	}
	if w = call("DELETE", "/api/admin/mail-addresses/sales@example.test", token, csrf, confirm("")); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"reserved"`) {
		t.Fatal("release", w.Code, w.Body)
	}
	if w = call("DELETE", "/api/admin/mail-addresses/missing@example.test", token, csrf, confirm("")); w.Code != 404 {
		t.Fatal("unknown release", w.Code)
	}
	if w = call("POST", "/api/admin/mail-addresses/sales@example.test/reassign", token, csrf, confirm(`"mailbox":"`+second.ID+`",`)); w.Code != 200 || !strings.Contains(w.Body.String(), `"generation":3`) {
		t.Fatal("reassign", w.Code, w.Body)
	}
	// Administrator subjects own no aliases.
	promoted := scimUser("native-runtime-two", "runtime-two", true, "kypost.admin")
	promoted["emails"] = two["emails"]
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "address-two-promoted", 2, promoted))
	if w = add(second.ID, "boss@example.test"); w.Code != 409 || !strings.Contains(w.Body.String(), "administrator") {
		t.Fatal("alias for an administrator", w.Code, w.Body)
	}
}

// The native send From gate: only an active address of the caller's own
// mailbox passes; anything else stays 403 before any outbox work.
func TestNativeSendFromAcceptsOnlyOwnedActiveAddresses(t *testing.T) {
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "from-one", 1, runtimeDirectoryUser(true)))
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "from-two", 1, two))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, second.ID, "other@example.test"); err != nil {
		t.Fatal(err)
	}
	send := func(from string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/mail/send", strings.NewReader(`{"to":"visible@outside.test","subject":"s","body":"b","from":"`+from+`"}`))
		req.Header.Set("Content-Type", "application/json")
		authRequestAs(srv, req, one.ID)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	for _, from := range []string{"other@example.test", "two@example.test", "nobody@example.test"} {
		if w := send(from); w.Code != 403 {
			t.Fatal("foreign From passed", from, w.Code, w.Body)
		}
	}
	// No relay is configured, so an accepted From ends in the outbox's 503.
	for _, from := range []string{"Sales@Example.test", "one@example.test", ""} {
		if w := send(from); w.Code != 503 || strings.Contains(w.Body.String(), "active address") {
			t.Fatal("owned From refused at the gate", from, w.Code, w.Body)
		}
	}
	if _, err = srv.ssoLifecycle.ReleaseNativeAlias(ctx, srv.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if w := send("sales@example.test"); w.Code != 403 {
		t.Fatal("released alias passed", w.Code, w.Body)
	}
}

// A committed release whose route write fails is still a success: 200 with
// the committed record and a warning; a retry sees the reserved address.
func TestNativeMailAddressReleaseWithPendingRoute(t *testing.T) {
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "pending-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-addresses"
	admin, err := srv.users.Create(context.Background(), "pending-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	if _, err = srv.ssoLifecycle.AddNativeAlias(context.Background(), srv.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	// A route ahead of the ledger makes the inactive-route write refuse.
	holding, err := ingress.Open(filepath.Join(srv.stateDir, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer holding.Close()
	if err = holding.SetRoute(context.Background(), ingress.Route{Address: "sales@example.test", Issuer: "https://idp.example", Subject: "native-runtime-one", Mailbox: one.ID, Generation: 50, Active: true, ValidUntil: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	release := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("DELETE", "/api/admin/mail-addresses/sales@example.test", strings.NewReader(`{"password":"`+password+`"}`))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	w := release()
	var got struct {
		State, Warning string
		Generation     int64
	}
	if err = json.Unmarshal(w.Body.Bytes(), &got); w.Code != 200 || err != nil || got.State != "reserved" || got.Generation != 2 || got.Warning != sso.ErrNativeRoutesPending.Error() {
		t.Fatal("committed release with a pending route", w.Code, w.Body)
	}
	addresses, err := srv.ssoLifecycle.NativeAddresses()
	if err != nil || addresses["sales@example.test"].State != "reserved" {
		t.Fatal("ledger change not durable", addresses, err)
	}
	if w = release(); w.Code != 409 {
		t.Fatal("retry of a committed release", w.Code, w.Body)
	}
	if err = srv.ssoLifecycle.ReconcileNativeAddresses(context.Background()); !errors.Is(err, sso.ErrNativeRoutesPending) {
		t.Fatal("worker pass did not retry the pending route", err)
	}
}
