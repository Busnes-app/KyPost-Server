//go:build linux

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestQuarantineAdminAPI(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "quarantine-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := srv.nativeMailAssignment(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Two deliveries frozen to one's mailbox, quarantined by a newer route.
	holding, err := ingress.Open(filepath.Join(srv.stateDir, "receiving"), ingress.ReceivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	route := ingress.Route{Address: a.Address, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Minute)}
	if err = holding.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	raw := "From: header-from@outside.test\r\nSubject: secret-subject\r\n\r\nsecret-body\r\n"
	for _, id := range []string{"keep", "drop"} {
		if err = holding.Bind(ctx, "maddy-local", id, "envelope@outside.test", a.Address); err != nil {
			t.Fatal(err)
		}
		if err = holding.Accept(ctx, "maddy-local", id, "envelope@outside.test", strings.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	route.Generation = 2
	if err = holding.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"keep", "drop"} {
		if _, err = holding.Claim(ctx, "maddy-local", id, time.Minute); err != ingress.ErrRoute {
			t.Fatal("not quarantined", err)
		}
	}
	_ = holding.Close()

	const password = "long-password-for-quarantine"
	admin, err := srv.users.Create(ctx, "quarantine-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if srv.logger, err = logging.NewWithOutput(&logs); err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	mtoken, mcsrf := mintSessionForTest(srv, one.ID)
	call := func(method, path, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	const base = "/api/admin/receiving/quarantine"
	if w := call("GET", base, mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member listed quarantine", w.Code)
	}
	for _, path := range []string{base + "/maddy-local/keep/release", base + "/maddy-local/drop/discard"} {
		if w := call("POST", path, mtoken, mcsrf, `{"password":"x"}`); w.Code != 403 {
			t.Fatal("member changed quarantine", path, w.Code)
		}
		if w := call("POST", path, token, csrf, `{}`); w.Code != 403 || !strings.Contains(w.Body.String(), "invalid credentials") {
			t.Fatal("no step-up accepted", path, w.Code)
		}
		if w := call("POST", path, token, csrf, `{"password":"wrong-password-for-quarantine"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "invalid credentials") {
			t.Fatal("wrong step-up accepted", path, w.Code)
		}
	}
	w := call("GET", base, token, csrf, "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"id":"keep"`) || !strings.Contains(body, `"id":"drop"`) || !strings.Contains(body, `"sender":"envelope@outside.test"`) || !strings.Contains(body, `"user":"`+one.ID+`"`) || !strings.Contains(body, `"mailbox":"`+one.ID+`"`) {
		t.Fatal("list", w.Code, body)
	}
	for _, secret := range []string{"secret-subject", "secret-body", "header-from"} {
		if strings.Contains(body, secret) {
			t.Fatal("listing leaked message content", secret)
		}
	}
	confirm := `{"password":"` + password + `"}`
	if w = call("POST", base+"/maddy-local/missing/release", token, csrf, confirm); w.Code != 404 {
		t.Fatal("missing release", w.Code, w.Body)
	}
	if w = call("POST", base+"/maddy-local/"+strings.Repeat("x", 257)+"/discard", token, csrf, confirm); w.Code != 404 || strings.Contains(logs.String(), "xxxxxxxx") {
		t.Fatal("overlong id looked up or logged", w.Code, w.Body)
	}
	hold := filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile)
	if err = os.WriteFile(hold, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w = call("POST", base+"/maddy-local/drop/discard", token, csrf, confirm); w.Code != 409 {
		t.Fatal("discard under restore hold", w.Code, w.Body)
	}
	if err = os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if w = call("POST", base+"/maddy-local/keep/release", token, csrf, confirm); w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"released"`) {
		t.Fatal("release", w.Code, w.Body)
	}
	if w = call("POST", base+"/maddy-local/drop/discard", token, csrf, confirm); w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"discarded"`) {
		t.Fatal("discard", w.Code, w.Body)
	}
	if w = call("POST", base+"/maddy-local/drop/release", token, csrf, confirm); w.Code != 409 {
		t.Fatal("discarded delivery released", w.Code, w.Body)
	}
	if w = call("GET", base, token, csrf, ""); w.Code != 200 || strings.Contains(w.Body.String(), `"id":`) {
		t.Fatal("list after", w.Code, w.Body)
	}
	audit := logs.String()
	for _, want := range []string{"maddy-local/keep", "released", "maddy-local/drop", "discarded", admin.ID} {
		if !strings.Contains(audit, want) {
			t.Fatal("audit lacks", want, audit)
		}
	}
	for _, secret := range []string{"secret-subject", "secret-body", "header-from", "envelope@outside.test"} {
		if strings.Contains(audit, secret) {
			t.Fatal("audit logged correspondence", secret)
		}
	}
	box, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if rows, err := box.List(ctx, "INBOX", 0, 10); err != nil || len(rows) != 1 {
		t.Fatal("released mail not delivered exactly once", len(rows), err)
	}
}

// The Cloudflare status is admin-only, counts-only, and reports a host
// without its host record as fenced.
func TestCloudflareReceivingStatusAPI(t *testing.T) {
	srv := newNativeRuntimeServer(t)
	secret := config.SecretDir()
	admin, err := srv.users.Create(context.Background(), "cloudflare-admin", "long-password-for-cloudflare", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.users.Create(context.Background(), "cloudflare-member", "long-password-for-cloudflare", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) *httptest.ResponseRecorder {
		token, csrf := mintSessionForTest(srv, id)
		req := httptest.NewRequest("GET", "/api/admin/receiving/cloudflare", nil)
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	if w := get(member.ID); w.Code != 403 {
		t.Fatal("member read receiving status", w.Code)
	}
	if w := get(admin.ID); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"uninitialized"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	keys := cfreceiving.Keys{Dir: secret}
	m, err := keys.Init()
	if err != nil {
		t.Fatal(err)
	}
	if w := get(admin.ID); !strings.Contains(w.Body.String(), `"state":"not-started"`) || strings.Contains(w.Body.String(), m.Token) {
		t.Fatal(w.Body.String())
	}
	if err := os.Remove(filepath.Join(secret, cfreceiving.HostFile)); err != nil {
		t.Fatal(err)
	}
	if w := get(admin.ID); !strings.Contains(w.Body.String(), `"state":"fenced"`) {
		t.Fatal("restored host not reported fenced", w.Body.String())
	}
}

// An unresolved delivery (owner unknown, e.g. after a restore) is listed with
// today's owner and released only with the explicit flag, audited apart.
func TestQuarantineReleaseToCurrentOwnerAPI(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "unresolved-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := srv.nativeMailAssignment(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(srv.stateDir, "receiving"), ingress.ReceivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	two := scimUser("native-runtime-two", "runtime-two", true)
	two["emails"] = []map[string]any{{"value": "two@example.test", "primary": true}}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "unresolved-two", 1, two))
	second, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	for id, address := range map[string]string{"waited": a.Address, "moved": "sales@example.test"} {
		if err = holding.Quarantine(ctx, "cloudflare-continuous", id, "envelope@outside.test", ingress.Binding{Address: address, Generation: 1}, []byte("Subject: secret-subject\r\n\r\nsecret-body\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	_ = holding.Close()
	const password = "long-password-for-unresolved"
	admin, err := srv.users.Create(ctx, "unresolved-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if srv.logger, err = logging.NewWithOutput(&logs); err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	const base = "/api/admin/receiving/quarantine"
	if w := call("GET", base, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"unresolved":true`) || !strings.Contains(w.Body.String(), `"currentMailbox":"`+one.ID+`"`) || !strings.Contains(w.Body.String(), `"currentUser":"`+one.ID+`"`) || !strings.Contains(w.Body.String(), `"mailbox":""`) {
		t.Fatal("unresolved listing", w.Code, w.Body)
	}
	plain, flagged := `{"password":"`+password+`"}`, `{"password":"`+password+`","toCurrentOwner":true,"currentMailbox":"`+one.ID+`"}`
	if w := call("POST", base+"/cloudflare-continuous/waited/release", plain); w.Code != 409 || !strings.Contains(w.Body.String(), "current owner") || strings.Contains(w.Body.String(), "resync and retry") {
		t.Fatal("unresolved released without the flag", w.Code, w.Body)
	}
	if w := call("POST", base+"/cloudflare-continuous/waited/discard", flagged); w.Code != 400 {
		t.Fatal("flag accepted on discard", w.Code)
	}
	for _, bad := range []string{`{"password":"` + password + `","toCurrentOwner":true}`, `{"password":"` + password + `","currentMailbox":"` + one.ID + `"}`} {
		if w := call("POST", base+"/cloudflare-continuous/waited/release", bad); w.Code != 400 {
			t.Fatal("release without the reviewed mailbox, or a mailbox without the flag", w.Code, w.Body)
		}
	}
	// Reviewed as one's in the list, then reassigned to two before confirming.
	if _, err = srv.ssoLifecycle.ReleaseNativeAlias(ctx, srv.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ssoLifecycle.ReassignNativeAddress(ctx, srv.stateDir, "sales@example.test", second.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", base+"/cloudflare-continuous/moved/release", flagged); w.Code != 409 || !strings.Contains(w.Body.String(), "changed since you reviewed it") {
		t.Fatal("released to an owner the administrator never reviewed", w.Code, w.Body)
	}
	if w := call("POST", base+"/cloudflare-continuous/waited/release", flagged); w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"released"`) {
		t.Fatal("release to current owner", w.Code, w.Body)
	}
	if !strings.Contains(logs.String(), "release_quarantine_to_current_owner") || strings.Contains(logs.String(), "secret-") {
		t.Fatal("distinct audit", logs.String())
	}
	box, err := mailbox.OpenExisting(filepath.Join(a.Dir(srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if rows, err := box.List(ctx, "INBOX", 0, 10); err != nil || len(rows) != 1 {
		t.Fatal("not delivered to the current owner exactly once", len(rows), err)
	}
}

func TestSenderBlocksAdminAPI(t *testing.T) {
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "blocks-one", 1, runtimeDirectoryUser(true)))
	one, err := srv.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-sender-blocks"
	admin, err := srv.users.Create(context.Background(), "blocks-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if srv.logger, err = logging.NewWithOutput(&logs); err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	mtoken, mcsrf := mintSessionForTest(srv, one.ID)
	call := func(method, path, token, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	const base = "/api/admin/receiving/blocks"
	confirm := `"password":"` + password + `"`
	// Before receiving init: empty list, and changes refuse without creating the spool.
	if w := call("GET", base, token, csrf, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"blocks":[],"evidence":{"damaged":false,"resetAt":null,"domainBlocksFrom":null,"goodFull":false,"automaticFull":false}}` {
		t.Fatal("empty list", w.Code, w.Body)
	}
	if w := call("POST", base, token, csrf, `{"kind":"domain","value":"evil.test",`+confirm+`}`); w.Code != 409 {
		t.Fatal("block before receiving init", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(srv.stateDir, "receiving")); !os.IsNotExist(err) {
		t.Fatal("block created the receiving directory", err)
	}
	holding, err := ingress.Open(filepath.Join(srv.stateDir, "receiving"), ingress.ReceivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	_ = holding.Close()

	// AuthZ and step-up.
	if w := call("GET", base, mtoken, mcsrf, ""); w.Code != 403 {
		t.Fatal("member listed blocks", w.Code)
	}
	for _, c := range [][2]string{{"POST", base}, {"DELETE", base + "/" + ingress.BlockID("domain", "evil.test")}} {
		if w := call(c[0], c[1], mtoken, mcsrf, `{"kind":"domain","value":"evil.test","password":"x"}`); w.Code != 403 {
			t.Fatal("member changed blocks", c, w.Code)
		}
		if w := call(c[0], c[1], token, csrf, `{"kind":"domain","value":"evil.test"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "invalid credentials") {
			t.Fatal("no step-up accepted", c, w.Code)
		}
		if w := call(c[0], c[1], token, csrf, `{"kind":"domain","value":"evil.test","password":"wrong-password-for-blocks"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "invalid credentials") {
			t.Fatal("wrong step-up accepted", c, w.Code)
		}
	}
	// Refusals: own domain or address on it, non-ASCII, malformed, free text.
	for body, code := range map[string]int{
		`{"kind":"domain","value":"Example.test",`:               409,
		`{"kind":"address","value":"anyone@example.test",`:       409,
		`{"kind":"domain","value":"bücher.test",`:                400,
		`{"kind":"address","value":"Name <a@x.test>",`:           400,
		`{"kind":"user","value":"a@x.test",`:                     400,
		`{"kind":"domain","value":"evil.test","reason":"hello",`: 400,
		`{"kind":"domain","value":"evil.test","until":1,`:        400,
	} {
		if w := call("POST", base, token, csrf, body+confirm+`}`); w.Code != code {
			t.Fatal("refusal", body, w.Code, w.Body)
		}
	}
	until := time.Now().Add(time.Hour).UnixMilli()
	w := call("POST", base, token, csrf, `{"kind":"address","value":"Bad@Spam.test","reason":"phishing","until":`+strconv.FormatInt(until, 10)+`,`+confirm+`}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":"bad@spam.test"`) || !strings.Contains(w.Body.String(), `"actor":"`+admin.ID+`"`) || !strings.Contains(w.Body.String(), `"source":"manual"`) {
		t.Fatal("block", w.Code, w.Body)
	}
	if w = call("POST", base, token, csrf, `{"kind":"domain","value":"evil.test",`+confirm+`}`); w.Code != 200 {
		t.Fatal("block domain", w.Code, w.Body)
	}
	// Adding a blocked domain, or one with a blocked address, as a mail domain is refused.
	if w = call("POST", base, token, csrf, `{"kind":"address","value":"someone@blocked-addr.test",`+confirm+`}`); w.Code != 200 {
		t.Fatal("block address", w.Code, w.Body)
	}
	for _, c := range [][3]string{{"POST", "/api/admin/mail-domains", "Evil.test"}, {"POST", "/api/admin/mail-domains", "blocked-addr.test"}, {"PUT", "/api/admin/mail-domain", "evil.test"}} {
		if w := call(c[0], c[1], token, csrf, `{"domain":"`+c[2]+`",`+confirm+`}`); w.Code != 409 || !strings.Contains(w.Body.String(), "unblock it first") {
			t.Fatal("blocked domain added", c, w.Code, w.Body)
		}
	}
	if w := call("POST", "/api/admin/mail-domains", token, csrf, `{"domain":"clean.test",`+confirm+`}`); strings.Contains(w.Body.String(), "unblock") {
		t.Fatal("unblocked domain refused for a block", w.Code, w.Body)
	}
	w = call("GET", base, token, csrf, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":"bad@spam.test"`) || !strings.Contains(w.Body.String(), `"value":"evil.test"`) {
		t.Fatal("list", w.Code, w.Body)
	}
	badID := ingress.BlockID("address", "bad@spam.test")
	if !strings.Contains(w.Body.String(), `"id":"`+badID+`"`) {
		t.Fatal("list lacks the block id", w.Body)
	}
	for _, path := range []string{base + "/address/bad@spam.test", base + "/bad@spam.test", base + "/" + strings.ToUpper(badID)} {
		if w = call("DELETE", path, token, csrf, `{`+confirm+`}`); w.Code != 400 && w.Code != 404 && w.Code != 405 || strings.Contains(w.Body.String(), "unblocked") {
			t.Fatal("unblock by address", path, w.Code, w.Body)
		}
	}
	// Evidence that cannot record the suppression never keeps a block: the
	// unblock succeeds with a warning.
	evidence := filepath.Join(srv.stateDir, "receiving", ingress.EvidenceFile)
	_ = os.Remove(evidence)
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	if w = call("DELETE", base+"/"+badID, token, csrf, `{`+confirm+`}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"unblocked"`) || !strings.Contains(w.Body.String(), `"warning":`) {
		t.Fatal("unblock", w.Code, w.Body)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
	if w = call("DELETE", base+"/"+badID, token, csrf, `{`+confirm+`}`); w.Code != 404 {
		t.Fatal("second unblock", w.Code, w.Body)
	}
	if w = call("GET", base, token, csrf, ""); strings.Contains(w.Body.String(), "bad@spam.test") || !strings.Contains(w.Body.String(), "evil.test") {
		t.Fatal("list after unblock", w.Body)
	}
	// Audited with actor, action and block ID; never the address.
	audit := logs.String()
	for _, want := range []string{`"action":"block_sender"`, `"action":"unblock_sender"`, `"result":"blocked"`, `"result":"unblocked"`, `"result":"refused"`, `"actor":"` + admin.ID + `"`, `"correlation_id":"` + badID + `"`, `"result":"refused","correlation_id":"` + ingress.BlockID("domain", "example.test") + `"`} {
		if !strings.Contains(audit, want) {
			t.Fatal("audit missing", want, audit)
		}
	}
	if strings.Contains(audit, "spam.test") || strings.Contains(audit, "evil.test") || strings.Contains(audit, "blocked-addr") {
		t.Fatal("audit logged a sender", audit)
	}
}
