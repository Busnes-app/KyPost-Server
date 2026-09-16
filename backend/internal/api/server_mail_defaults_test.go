package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// adminAndMember returns a server with an admin and a plain member.
// newTestServerWithUser creates a RoleUser, so promote it first.
func adminAndMember(t *testing.T) (*Server, users.User, users.User) {
	t.Helper()
	srv, admin := newTestServerWithUser(t)
	if _, err := srv.users.SetRole(admin.ID, users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	member, err := srv.users.Create(context.Background(), "member", "member-password-1234", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	return srv, admin, member
}

func TestMailDefaultsAdminWritesUserReads(t *testing.T) {
	srv, admin, member := adminAndMember(t)

	body, _ := json.Marshal(map[string]any{"host": " imap.example.test ", "port": 993, "smtpHost": "smtp.example.test", "smtpPort": 587})
	req := httptest.NewRequest(http.MethodPut, "/api/mail-defaults", bytes.NewReader(body))
	authRequestAs(srv, req, admin.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/mail-defaults", nil)
	authRequestAs(srv, req, member.ID)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	var got mailDefaults
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Host != "imap.example.test" || got.SMTPPort != 587 {
		t.Fatalf("member GET status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMailDefaultsUserCannotWrite(t *testing.T) {
	srv, _, member := adminAndMember(t)
	body, _ := json.Marshal(map[string]any{"host": "x"})
	req := httptest.NewRequest(http.MethodPut, "/api/mail-defaults", bytes.NewReader(body))
	authRequestAs(srv, req, member.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

func TestMailDefaultsEmptyWhenUnset(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	req := httptest.NewRequest(http.MethodGet, "/api/mail-defaults", nil)
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() == "" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
