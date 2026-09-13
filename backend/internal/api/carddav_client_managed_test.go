package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func seedManagedCardDAV(t *testing.T, srv *Server, userID string, managed bool) {
	t.Helper()
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	if err := writeCardDAVClientConfigPayload(srv.userCardDAVClientConfigPath(userID), srv.imapConfigKeyPath, carddavClientConfigPayload{
		ServerURL: "https://contacts.example.test/dav/", Username: "gwen", Password: "secret", Managed: managed,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestCardDAVClientGETReportsManaged(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedCardDAV(t, srv, u.ID, true)
	req := httptest.NewRequest(http.MethodGet, "/api/contacts/carddav-client/config", nil)
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["managed"] != true {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCardDAVClientPOSTAndDELETERefusedWhenManaged(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	seedManagedCardDAV(t, srv, u.ID, true)
	body, _ := json.Marshal(map[string]any{"serverUrl": "https://evil.test/", "username": "x", "password": "y"})
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/carddav-client/config", bytes.NewReader(body))
	authRequestAs(srv, req, u.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403", rec.Code)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/contacts/carddav-client/config", nil)
	authRequestAs(srv, req, u.ID)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE status = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(srv.userCardDAVClientConfigPath(u.ID)); err != nil {
		t.Fatalf("config removed despite lock: %v", err)
	}
}
