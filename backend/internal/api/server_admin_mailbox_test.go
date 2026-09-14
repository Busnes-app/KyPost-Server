package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/logging"
	"github.com/Busness-app/kypost-server/backend/internal/mailmsg"
)

// adminAndMember comes from server_mail_defaults_test.go (Task 3).

func doJSONAs(t *testing.T, srv *Server, asUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	authRequestAs(srv, req, asUser)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestAdminAssignsManagedIMAPConfig(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "member@example.test", "password": "s3cret", "managed": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, exists, err := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath)
	if err != nil || !exists || !stored.Managed || stored.Password != "s3cret" || stored.Port != 993 {
		t.Fatalf("stored=%+v exists=%v err=%v", stored, exists, err)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, leaked := body["password"]; leaked {
		t.Fatal("password echoed to admin")
	}
}

func TestAdminBlankPasswordKeepsStored(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "keep-me", "managed": true,
	})
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "", "managed": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _, _ := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath)
	if stored.Password != "keep-me" || stored.Managed {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestAdminCannotRepointStoredCredential(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "orig-pw", "smtpHost": "smtp.example.test",
	})

	// Different host, blank password -> refused, stored destination and
	// password untouched.
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.evil.test", "username": "m", "password": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("different host: status=%d, want 400", rec.Code)
	}
	stored, _, _ := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath)
	if stored.Host != "imap.example.test" || stored.Password != "orig-pw" {
		t.Fatalf("stored mutated: %+v", stored)
	}

	// Different smtpHost, blank password -> refused, stored untouched.
	rec = doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "", "smtpHost": "smtp.evil.test",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("different smtpHost: status=%d, want 400", rec.Code)
	}
	stored, _, _ = mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath)
	if stored.SMTPHost != "smtp.example.test" || stored.Password != "orig-pw" {
		t.Fatalf("stored mutated: %+v", stored)
	}

	// Same destination, managed toggled, blank password -> allowed, password
	// retained. The convenience the lock flip depends on must still work.
	rec = doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "", "smtpHost": "smtp.example.test", "managed": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("same destination: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _, _ = mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath)
	if stored.Password != "orig-pw" || !stored.Managed {
		t.Fatalf("stored=%+v", stored)
	}

	// CardDAV: different serverUrl, blank password -> refused, stored untouched.
	allowLoopbackOutboundForTest(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer fake.Close()
	fake2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer fake2.Close()

	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake.URL + "/dav/", "username": "m", "password": "cd-pw",
	})
	rec = doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake2.URL + "/dav/", "username": "m", "password": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("different serverUrl: status=%d, want 400", rec.Code)
	}
	cdStored, _, _ := readCardDAVClientConfigPayload(srv.userCardDAVClientConfigPath(member.ID), srv.imapConfigKeyPath)
	if cdStored.ServerURL != fake.URL+"/dav/" || cdStored.Password != "cd-pw" {
		t.Fatalf("carddav stored mutated: %+v", cdStored)
	}
}

func TestAdminCardDAVLogOmitsURLCredentials(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	var buf bytes.Buffer
	logger, err := logging.NewWithOutput(&buf)
	if err != nil {
		t.Fatalf("logging.NewWithOutput: %v", err)
	}
	srv.logger = logger

	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": "https://gwen:hunter2-distinctive@contacts.example.test/dav/", "username": "gwen", "password": "p",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("userinfo url: status=%d, want 400", rec.Code)
	}
	if bytes.Contains(buf.Bytes(), []byte("hunter2-distinctive")) {
		t.Fatalf("credential leaked into log: %s", buf.String())
	}

	buf.Reset()
	allowLoopbackOutboundForTest(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer fake.Close()
	rec = doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake.URL + "/dav/", "username": "m", "password": "s3cret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("user_id")) || !bytes.Contains(buf.Bytes(), []byte("admin_id")) {
		t.Fatalf("expected audit fields in log: %s", buf.String())
	}
	if bytes.Contains(buf.Bytes(), []byte(fake.URL)) {
		t.Fatalf("server url leaked into log: %s", buf.String())
	}
}

func TestAdminIMAPConfigRequiresPasswordOnFirstWrite(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestAdminIMAPConfigUnknownUser404(t *testing.T) {
	srv, admin, _ := adminAndMember(t)
	rec := doJSONAs(t, srv, admin.ID, http.MethodGet, "/api/users/nope/imap-config", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestMemberCannotUseAdminIMAPRoute(t *testing.T) {
	srv, _, member := adminAndMember(t)
	rec := doJSONAs(t, srv, member.ID, http.MethodGet, "/api/users/"+member.ID+"/imap-config", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

func TestAdminUnlockLetsUserEditAgain(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "p", "managed": true,
	})
	if rec := doJSONAs(t, srv, member.ID, http.MethodPost, "/api/imap/config", map[string]any{"host": "other.test", "username": "m", "password": "p"}); rec.Code != http.StatusForbidden {
		t.Fatalf("locked: status=%d", rec.Code)
	}
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "managed": false,
	})
	if rec := doJSONAs(t, srv, member.ID, http.MethodPost, "/api/imap/config", map[string]any{"host": "other.test", "username": "m", "password": "p"}); rec.Code != http.StatusOK {
		t.Fatalf("unlocked: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminDeletesIMAPConfig(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/imap-config", map[string]any{
		"host": "imap.example.test", "username": "m", "password": "p", "managed": true,
	})
	if rec := doJSONAs(t, srv, admin.ID, http.MethodDelete, "/api/users/"+member.ID+"/imap-config", nil); rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if _, exists, _ := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(member.ID), srv.imapConfigKeyPath); exists {
		t.Fatal("config still present")
	}
}

func TestAdminAssignsManagedCardDAVClient(t *testing.T) {
	allowLoopbackOutboundForTest(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer fake.Close()
	srv, admin, member := adminAndMember(t)
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake.URL + "/dav/", "username": "m", "password": "s3cret", "managed": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, exists, err := readCardDAVClientConfigPayload(srv.userCardDAVClientConfigPath(member.ID), srv.imapConfigKeyPath)
	if err != nil || !exists || !stored.Managed || stored.Password != "s3cret" {
		t.Fatalf("stored=%+v exists=%v err=%v", stored, exists, err)
	}
	if rec := doJSONAs(t, srv, member.ID, http.MethodDelete, "/api/contacts/carddav-client/config", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("member delete while locked: status=%d", rec.Code)
	}
}

func TestAdminCardDAVClientRejectsPlainHTTP(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": "http://contacts.example.test/dav/", "username": "m", "password": "p",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestAdminCardDAVClientBlankPasswordKeepsStored(t *testing.T) {
	allowLoopbackOutboundForTest(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer fake.Close()
	srv, admin, member := adminAndMember(t)
	doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake.URL + "/dav/", "username": "m", "password": "keep-me", "managed": true,
	})

	// Seed sync state as if an unattended sync had already run, the way it
	// would between two admin edits in production.
	cfgPath := srv.userCardDAVClientConfigPath(member.ID)
	seeded, _, err := readCardDAVClientConfigPayload(cfgPath, srv.imapConfigKeyPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	seeded.LastSyncedAt = "2026-01-01T00:00:00Z"
	seeded.LastSyncImported = 3
	seeded.AddressBookPath = "/carddav/33/contacts/main/"
	if err := writeCardDAVClientConfigPayload(cfgPath, srv.imapConfigKeyPath, seeded); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	rec := doJSONAs(t, srv, admin.ID, http.MethodPut, "/api/users/"+member.ID+"/carddav-client", map[string]any{
		"serverUrl": fake.URL + "/dav/", "username": "m", "managed": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _, _ := readCardDAVClientConfigPayload(cfgPath, srv.imapConfigKeyPath)
	if stored.Password != "keep-me" || stored.Managed {
		t.Fatalf("stored=%+v", stored)
	}
	if stored.LastSyncedAt != "2026-01-01T00:00:00Z" || stored.LastSyncImported != 3 || stored.AddressBookPath != "/carddav/33/contacts/main/" {
		t.Fatalf("sync state discarded: stored=%+v", stored)
	}
}
