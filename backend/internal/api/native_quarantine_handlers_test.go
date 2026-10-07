//go:build linux

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		if w := call("POST", path, token, csrf, `{}`); w.Code != 401 {
			t.Fatal("no step-up accepted", path, w.Code)
		}
		if w := call("POST", path, token, csrf, `{"password":"wrong-password-for-quarantine"}`); w.Code != 401 {
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
