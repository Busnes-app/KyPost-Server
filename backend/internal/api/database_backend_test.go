package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// AC11: /api/health and /api/status name the backend and never the database's address
// or credentials.
func TestHealthAndStatusReportDatabaseBackend(t *testing.T) {
	srv := newTestServer(t)
	u, err := users0(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		if path == "/api/health" {
			srv.handleHealth(rec, req)
		} else {
			srv.handleStatus(rec, authedRequestForTest(req, u))
		}
		if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	// Opens the user's store under the test's own backend, so the swaps below change
	// only what the endpoints report.
	for _, path := range []string{"/api/health", "/api/status"} {
		if body := get(path); !strings.Contains(body, `"databaseBackend":"`+dbbackend.Name()+`"`) {
			t.Fatalf("%s: %s", path, body)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "")
		for _, path := range []string{"/api/health", "/api/status"} {
			if body := get(path); !strings.Contains(body, `"databaseBackend":"sqlite"`) {
				t.Fatalf("%s: %s", path, body)
			}
		}
	})
	t.Run("postgres", func(t *testing.T) {
		dbbackend.SetURLForTest(t, "postgres://kypost:hunter2-secret@db.internal.example:6432/kypost?sslmode=verify-full")
		for _, path := range []string{"/api/health", "/api/status"} {
			body := get(path)
			if !strings.Contains(body, `"databaseBackend":"postgres"`) {
				t.Fatalf("%s: %s", path, body)
			}
			for _, secret := range []string{"hunter2-secret", "db.internal.example", "6432", "postgres://"} {
				if strings.Contains(body, secret) {
					t.Fatalf("%s leaks %q: %s", path, secret, body)
				}
			}
		}
	})
}
