package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordSnapshotForcedChangePreservesPGP(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "onboarding", true: "admin-reset"}[withKey], func(t *testing.T) {
			srv, u := newTestServerWithUser(t)
			if withKey {
				u = clientProtectedUser(t, srv)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/auth/password", nil)
			authRequestAs(srv, req, u.ID)
			const temporary = "temporary-reset-password"
			before, err := srv.users.SetPassword(context.Background(), u.ID, temporary, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
			}
			var snapshot struct {
				PGPRevision        uint64 `json:"pgpRevision"`
				WrappedPrivateKey  string `json:"wrappedPrivateKey"`
				MustChangePassword bool   `json:"mustChangePassword"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.PGPRevision != before.PGPRevision || !snapshot.MustChangePassword || snapshot.WrappedPrivateKey != "" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("bad snapshot: %s", rec.Body.String())
			}
			blocked := httptest.NewRequest(http.MethodGet, "/api/pgp/bootstrap", nil)
			blocked.Header = req.Header.Clone()
			rec = httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, blocked)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("bootstrap: %d", rec.Code)
			}
			body, err := json.Marshal(map[string]any{
				"oldPassword": temporary, "newAuthSecret": strings.Repeat("a", 64),
				"newLoginSalt":  base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
				"newIterations": 600000, "expectedRevision": snapshot.PGPRevision,
			})
			if err != nil {
				t.Fatal(err)
			}
			change := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader(body))
			change.Header = req.Header.Clone()
			rec = httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, change)
			if rec.Code != http.StatusOK {
				t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
			}
			after, err := srv.users.Get(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.MustChangePassword || after.PGPPrivateKeyWrapped != before.PGPPrivateKeyWrapped || after.PGPRevision != before.PGPRevision+1 {
				t.Fatal("forced change lost PGP material or failed to advance revision")
			}
			rec = httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("ordinary snapshot: %d", rec.Code)
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.MustChangePassword || snapshot.PGPRevision != after.PGPRevision || snapshot.WrappedPrivateKey != after.PGPPrivateKeyWrapped {
				t.Fatal("ordinary snapshot is not the current user's envelope/revision")
			}
		})
	}
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/password", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous snapshot: %d", rec.Code)
	}
}
