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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
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
		}{{"", "", 401}, {mtoken, mcsrf, 403}, {token, "", 403}, {token, csrf, 403}} {
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
	put := func(mailboxID, subject string) string {
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
		id, err := store.Import(ctx, mailbox.Receipt{Gateway: "qualified-test", Delivery: subject, Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return "n1:" + store.MessageReferenceGeneration() + ":" + strconv.FormatInt(id, 10)
	}
	put(one.ID, "primary-only-subject")
	extraRef := put(extra.ID, "extra-only-subject")
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
	refused(one.ID, "mbx-00000000-0000-4000-8000-000000000000")
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
			ID, Kind   string
			Addresses  []struct{ Address, Kind string }
			UsedBytes  *int64
			QuotaBytes int64
		}
	}
	w = as(one.ID, "GET", "/api/mailboxes", "mbx-unknown")
	if err = json.Unmarshal(w.Body.Bytes(), &mine); w.Code != 200 || err != nil || len(mine.Mailboxes) != 2 || mine.Mailboxes[0].ID != one.ID || mine.Mailboxes[1].ID != extra.ID || mine.Mailboxes[1].Addresses[0].Address != "sales@example.test" {
		t.Fatal("caller mailboxes", w.Code, w.Body)
	}
	// Each mailbox reports what it stores against its quota.
	for _, m := range mine.Mailboxes {
		if m.UsedBytes == nil || *m.UsedBytes <= 0 || m.QuotaBytes != nativeMailboxLimits.PayloadBytes {
			t.Fatal("caller usage", w.Body)
		}
	}
	// Administrators see the same per mailbox, and the quotas against the
	// state filesystem: unused quota above 80% of the space beyond the reserve warns.
	var storage struct {
		Mailboxes []struct {
			Mailbox    string
			UsedBytes  *int64
			QuotaBytes int64
		}
		Storage struct {
			QuotaBytes, UsedBytes               int64
			FreeBytes, TotalBytes, ReserveBytes uint64
			Overcommitted                       bool
		}
	}
	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	for _, c := range []struct {
		free uint64
		over bool
	}{{10<<30 + uint64(float64(3*nativeMailboxLimits.PayloadBytes)/0.75), false}, {10<<30 + uint64(float64(3*nativeMailboxLimits.PayloadBytes)/0.85), true}} {
		fsutil.DiskSpace = func(string) (uint64, uint64, error) { return c.free, 100 << 30, nil }
		w = call("GET", "/api/admin/mail-addresses", token, csrf, "")
		if err = json.Unmarshal(w.Body.Bytes(), &storage); w.Code != 200 || err != nil {
			t.Fatal("admin storage", w.Code, w.Body)
		}
		var used int64
		for _, m := range storage.Mailboxes {
			if m.UsedBytes != nil {
				used += *m.UsedBytes
			}
		}
		if len(storage.Mailboxes) != 3 || used <= 0 || storage.Storage.UsedBytes != used || storage.Storage.QuotaBytes != 3*nativeMailboxLimits.PayloadBytes || storage.Storage.ReserveBytes != 10<<30 || storage.Storage.FreeBytes != c.free || storage.Storage.Overcommitted != c.over {
			t.Fatal("admin storage", c.free, w.Body)
		}
	}
	// Filtered to one user, the summary still covers the shared drive.
	w = call("GET", "/api/admin/mail-addresses?user="+second.ID, token, csrf, "")
	if err = json.Unmarshal(w.Body.Bytes(), &storage); w.Code != 200 || err != nil || len(storage.Mailboxes) != 1 || storage.Storage.QuotaBytes != 3*nativeMailboxLimits.PayloadBytes || !storage.Storage.Overcommitted {
		t.Fatal("filtered storage", w.Code, w.Body)
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

	// Sorter state is the primary's. A primary prediction under the same
	// numeric ID as the extra message must not learn from labelling it, nor
	// from the extra inbox sync that follows.
	settings := config.DefaultUserSettings()
	settings.Labels.Seeded = true
	settings.Labels.Allowlist = []string{"Primary", "Promotions"}
	if err := config.SaveUserSettings(srv.userSettingsPath(one.ID), settings); err != nil {
		t.Fatal(err)
	}
	primaryCache, err := srv.mailboxCacheStore(one.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	extraMailCache, err := srv.mailboxCacheStore(one.ID, extra.ID)
	if err != nil {
		t.Fatal(err)
	}
	primaryEntries, _, err := primaryCache.Snapshot(inboxCacheMailboxKey(""), 10)
	if err != nil {
		t.Fatal(err)
	}
	extraEntries, _, err := extraMailCache.Snapshot(inboxCacheMailboxKey(""), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(primaryEntries) != 1 || len(extraEntries) != 1 || primaryEntries[0].UID != extraEntries[0].UID {
		t.Fatal("fixture: the two mailboxes' first messages share a numeric ID", primaryEntries, extraEntries)
	}
	primaryState, err := srv.userStore(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	predict := func(e mailcache.Entry) {
		t.Helper()
		if err := primaryState.RecordSorterPrediction(strconv.Itoa(e.UID), state.SorterCheck(e.Sender, e.Subject), "m", []float32{1, 0}, "Primary"); err != nil {
			t.Fatal(err)
		}
	}
	untaught := func(step string) {
		t.Helper()
		if ex, err := primaryState.SorterCorrectionsStrict("m"); err != nil || len(ex) != 0 {
			t.Fatal(step+": extra mailbox taught the primary sorter", ex, err)
		}
	}
	generation, err := primaryState.SorterGeneration()
	if err != nil {
		t.Fatal(err)
	}
	predict(primaryEntries[0])
	label := httptest.NewRequest("POST", "/api/inbox/actions", strings.NewReader(`{"action":"label","keyword":"Promotions","messageIds":["`+extraRef+`"]}`))
	label.Header.Set("Content-Type", "application/json")
	authRequestAs(srv, label, one.ID)
	label.Header.Set(mailboxHeader, extra.ID)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, label)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"processed":1`) {
		t.Fatal("label in the extra mailbox", w.Code, w.Body)
	}
	untaught("label")
	predict(extraEntries[0])
	sees(as(one.ID, "GET", "/api/inbox?limit=10", extra.ID), "extra-only-subject", "primary-only-subject")
	untaught("sync")
	if after, err := primaryState.SorterGeneration(); err != nil || after != generation {
		t.Fatal("sorter generation changed", generation, after, err)
	}

	// The manual rules run acts on the selected mailbox only.
	rulesStore, err := srv.userRulesStore(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule := rulesTestRule("archive outside")
	rule.Match.Conditions[0].Value = "outside"
	if _, err := rulesStore.Upsert(rule); err != nil {
		t.Fatal(err)
	}
	run := httptest.NewRequest("POST", "/api/rules/run", strings.NewReader(`{"mailbox":"INBOX"}`))
	authRequestAs(srv, run, one.ID)
	run.Header.Set(mailboxHeader, extra.ID)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, run)
	var ran rulesRunResult
	if err := json.Unmarshal(w.Body.Bytes(), &ran); w.Code != 200 || err != nil || ran.Scanned != 1 || ran.Applied != 1 {
		t.Fatal("rules run in the extra mailbox", w.Code, w.Body)
	}
	sees(as(one.ID, "GET", "/api/inbox?limit=10", ""), "primary-only-subject", "extra-only-subject")
	if w := as(one.ID, "GET", "/api/inbox?limit=10", extra.ID); w.Code != 200 || strings.Contains(w.Body.String(), "extra-only-subject") {
		t.Fatal("rule did not archive the extra mailbox's message", w.Code, w.Body)
	}
}

// Incoming encryption covers the primary mailbox only, so it and extra
// mailboxes exclude each other in both directions.
func TestNativeExtraMailboxExcludesIncomingEncryption(t *testing.T) {
	ctx := context.Background()
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "exclusive-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-mailboxes"
	admin, err := srv.users.Create(ctx, "exclusive-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	create := func(address string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/admin/mailboxes", strings.NewReader(`{"user":"`+one.ID+`","address":"`+address+`","password":"`+password+`"}`))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	setEncrypt := func(on bool) {
		t.Helper()
		if err := config.UpdateUserSettings(srv.userSettingsPath(one.ID), func(s *config.UserSettings) error { s.EncryptIncoming = on; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setEncrypt(true)
	if w := create("sales@example.test"); w.Code != 409 || !strings.Contains(w.Body.String(), "incoming encryption") {
		t.Fatal("extra mailbox created beside incoming encryption", w.Code, w.Body)
	}
	setEncrypt(false)
	if _, err := srv.users.ReserveIncomingEncryption(one.ID, one.PGPFingerprint, one.PGPRevision); err != nil {
		t.Fatal(err)
	}
	if w := create("sales@example.test"); w.Code != 409 || !strings.Contains(w.Body.String(), "incoming encryption") {
		t.Fatal("extra mailbox created beside a pending replacement", w.Code, w.Body)
	}
	if err := srv.users.ReleaseIncomingEncryption(one.ID); err != nil {
		t.Fatal(err)
	}
	w := create("sales@example.test")
	var extra sso.NativeMailbox
	if err := json.Unmarshal(w.Body.Bytes(), &extra); w.Code != 200 || err != nil {
		t.Fatal("create", w.Code, w.Body)
	}
	enable := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/pgp/incoming", strings.NewReader(`{"enabled":true,"acknowledgeReplacement":true}`))
		req.Header.Set("Content-Type", "application/json")
		authRequestAs(srv, req, one.ID)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	want := `{"error":"` + errIncomingEncryptionExtraMailbox.Error() + `"}`
	if w := enable(); w.Code != 409 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatal("incoming encryption enabled beside an extra mailbox", w.Code, w.Body)
	}
	// Disabling every extra mailbox lifts the rule: the request passes the
	// mailbox gate and stops at step-up.
	if _, err := srv.ssoLifecycle.SetNativeMailboxState(ctx, srv.stateDir, extra.ID, false); err != nil {
		t.Fatal(err)
	}
	if w := enable(); w.Code == 409 || strings.Contains(w.Body.String(), "additional mailbox") {
		t.Fatal("a disabled extra mailbox still blocks incoming encryption", w.Code, w.Body)
	}
	toggle := func(action string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/admin/mailboxes/"+extra.ID+"/"+action, strings.NewReader(`{"password":"`+password+`"}`))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	setEncrypt(true)
	refused := `{"error":"` + errExtraMailboxIncomingEncryption.Error() + `"}`
	if w := toggle("enable"); w.Code != 409 || strings.TrimSpace(w.Body.String()) != refused {
		t.Fatal("extra mailbox enabled beside incoming encryption", w.Code, w.Body)
	}
	setEncrypt(false)
	if w := toggle("enable"); w.Code != 200 {
		t.Fatal("enable", w.Code, w.Body)
	}
	// An active mailbox whose creation never published storage is not listed
	// and does not block incoming encryption.
	path := filepath.Join(srv.configDir, "native-provisioning.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]any
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatal(err)
	}
	delete(ledger["mailboxes"].(map[string]any)[extra.ID].(map[string]any), "source")
	if raw, err = json.Marshal(ledger); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/mailboxes", nil)
	authRequestAs(srv, req, one.ID)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), extra.ID) {
		t.Fatal("unprepared mailbox listed", w.Code, w.Body)
	}
	if w := enable(); w.Code == 409 || strings.Contains(w.Body.String(), "additional mailbox") {
		t.Fatal("an unfinished extra mailbox blocks incoming encryption", w.Code, w.Body)
	}
	// A promoted subject selecting a mailbox gets the administrator 403.
	promoted := scimUser("native-runtime-one", "runtime-one", true, "kypost.admin")
	promoted["emails"] = runtimeDirectoryUser(true)["emails"]
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "exclusive-one-promoted", 2, promoted))
	req = httptest.NewRequest("GET", "/api/decisions", nil)
	authRequestAs(srv, req, one.ID)
	req.Header.Set(mailboxHeader, extra.ID)
	w = httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "administratorIdentity") {
		t.Fatal("administrator selecting a mailbox", w.Code, w.Body)
	}
}

// Enabling incoming encryption and creating or enabling an extra mailbox
// race through the real handlers; at most one may win. Both decide under the
// owner's settings lock, so neither interleaving leaves both on.
func TestNativeExtraMailboxIncomingEncryptionRace(t *testing.T) {
	ctx := context.Background()
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "race-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	userPassword := stepUpPassword(t, srv, one.ID)
	identity, err := pgpmail.GenerateIdentity("One", "one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	seedClientIdentity(t, srv, one.ID, identity.ArmoredPublicKey)
	if one, err = srv.users.Get(one.ID); err != nil {
		t.Fatal(err)
	}
	const adminPassword = "long-password-for-mailboxes"
	admin, err := srv.users.Create(ctx, "race-admin", adminPassword, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	routes := srv.routes()
	adminPost := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w
	}
	encrypt := func() *httptest.ResponseRecorder {
		body := `{"enabled":true,"acknowledgeReplacement":true,"expectedRevision":` + strconv.FormatUint(one.PGPRevision, 10) + `,"password":"` + userPassword + `"}`
		req := httptest.NewRequest("PUT", "/api/pgp/incoming", strings.NewReader(body))
		authRequestAs(srv, req, one.ID)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w
	}
	// reset turns encryption off and disables every extra mailbox.
	reset := func() {
		t.Helper()
		if err := config.UpdateUserSettings(srv.userSettingsPath(one.ID), func(s *config.UserSettings) error { s.EncryptIncoming = false; return nil }); err != nil {
			t.Fatal(err)
		}
		all, err := srv.ssoLifecycle.NativeMailboxes()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range all {
			if m.Kind == "extra" && m.State == "active" {
				if _, err := srv.ssoLifecycle.SetNativeMailboxState(ctx, srv.stateDir, m.ID, false); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// race starts the admin request delay after the encryption request, so the
	// rounds sweep the window in which either can win, then checks the invariant.
	race := func(delay time.Duration, admin func() *httptest.ResponseRecorder) (encrypted, extra bool) {
		t.Helper()
		var a, e *httptest.ResponseRecorder
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { <-start; time.Sleep(delay); a = admin(); close(done) }()
		close(start)
		e = encrypt()
		<-done
		if a.Code != 200 && a.Code != 409 || e.Code != 200 && e.Code != 409 {
			t.Fatal("unexpected answer", a.Code, a.Body, e.Code, e.Body)
		}
		settings, err := config.LoadUserSettings(srv.userSettingsPath(one.ID))
		if err != nil {
			t.Fatal(err)
		}
		active, err := srv.ownsActiveExtraMailbox(one.ID)
		if err != nil {
			t.Fatal(err)
		}
		if settings.EncryptIncoming && active {
			t.Fatal("incoming encryption and an active extra mailbox both on", a.Body, e.Body)
		}
		return settings.EncryptIncoming, active
	}
	reset()
	began := time.Now()
	if w := encrypt(); w.Code != 200 {
		t.Fatal("encryption cannot be enabled alone", w.Code, w.Body)
	}
	alone := time.Since(began)
	const rounds = 12
	// Delays sweep from 0 to twice the lone request's duration.
	delay := func(i int) time.Duration { return alone * time.Duration(2*i) / rounds }
	won := map[string]int{}
	var last string
	for i := range rounds {
		reset()
		address := "race" + strconv.Itoa(i) + "@example.test"
		enc, extra := race(delay(i), func() *httptest.ResponseRecorder {
			w := adminPost("/api/admin/mailboxes", `{"user":"`+one.ID+`","address":"`+address+`","password":"`+adminPassword+`"}`)
			var m sso.NativeMailbox
			if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &m) == nil {
				last = m.ID
			}
			return w
		})
		won["create:"+strconv.FormatBool(enc)+strconv.FormatBool(extra)]++
	}
	if last == "" || won["create:truefalse"] == 0 {
		t.Fatal("the rounds did not let both sides win", won)
	}
	for i := range rounds {
		reset()
		enc, extra := race(delay(i), func() *httptest.ResponseRecorder {
			return adminPost("/api/admin/mailboxes/"+last+"/enable", `{"password":"`+adminPassword+`"}`)
		})
		won["enable:"+strconv.FormatBool(enc)+strconv.FormatBool(extra)]++
	}
	if won["enable:falsetrue"] == 0 || won["enable:truefalse"] == 0 {
		t.Fatal("the rounds did not let both sides win", won)
	}
	t.Log("outcomes (encrypted, extra active):", won)
}
