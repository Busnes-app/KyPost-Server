package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/mailmsg"
)

func seedManagedIMAP(t *testing.T, srv *Server, userID string, managed bool) {
	t.Helper()
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.test", Port: 993, Username: "gwen", Password: "secret", Mailbox: "INBOX", Managed: managed,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestIMAPConfigGETReportsManaged(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedIMAP(t, srv, u.ID, true)
	req := httptest.NewRequest(http.MethodGet, "/api/imap/config", nil)
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["managed"] != true {
		t.Fatalf("managed = %v, want true", body["managed"])
	}
	if _, leaked := body["password"]; leaked {
		t.Fatal("password leaked in status")
	}
}

func TestIMAPConfigPOSTRefusedWhenManaged(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedIMAP(t, srv, u.ID, true)
	body, _ := json.Marshal(map[string]any{"host": "evil.test", "username": "x", "password": "y"})
	req := httptest.NewRequest(http.MethodPost, "/api/imap/config", bytes.NewReader(body))
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	stored, _, _ := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(u.ID), srv.imapConfigKeyPath)
	if stored.Host != "imap.example.test" {
		t.Fatalf("stored host changed to %q", stored.Host)
	}
}

func TestIMAPConfigDELETERefusedWhenManaged(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedIMAP(t, srv, u.ID, true)
	req := httptest.NewRequest(http.MethodDelete, "/api/imap/config", nil)
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(srv.userIMAPConfigPath(u.ID)); err != nil {
		t.Fatalf("config removed despite lock: %v", err)
	}
}

func TestIMAPConfigPOSTCannotSelfManage(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedIMAP(t, srv, u.ID, false)
	body, _ := json.Marshal(map[string]any{"host": "imap.example.test", "username": "gwen", "password": "p", "managed": true})
	req := httptest.NewRequest(http.MethodPost, "/api/imap/config", bytes.NewReader(body))
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	stored, _, _ := mailmsg.ReadIMAPConfigPayload(srv.userIMAPConfigPath(u.ID), srv.imapConfigKeyPath)
	if stored.Managed {
		t.Fatal("user was able to set managed on their own config")
	}
}
