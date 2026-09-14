package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

func TestPGPKeyringHTTPCompatibilityAndReset(t *testing.T) {
	srv := newTestServer(t)
	u := clientProtectedUser(t, srv)
	identity, err := pgpmail.GenerateIdentity("Ring API", "ring@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	info, err := pgpmail.InspectPublicKey(identity.ArmoredPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	u, err = srv.users.SetPGPIdentityClientProtected(u.ID, info.Fingerprint, info.KeyID, info.ArmoredPublicKey, `{"v":2}`, "generated", "now", nil)
	if err != nil {
		t.Fatal(err)
	}
	const authSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	salt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	u, err = srv.users.SetDerivedAuth(context.Background(), u.ID, authSecret, salt, 600000, false)
	if err != nil {
		t.Fatal(err)
	}
	u, err = srv.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{
		ExpectedRevision: u.PGPRevision, MaterialGeneration: 1, PrimaryFingerprints: []string{info.Fingerprint}, KeyFingerprints: info.KeyFingerprints,
		PublicKey: info.ArmoredPublicKey, PasswordEnvelope: `{"v":2,"password":"ring"}`, RecoveryEnvelope: `{"v":2,"recovery":"ring"}`, Source: "generated",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/pgp/bootstrap", "/api/pgp/identity", "/api/pgp/identity/wrapped", "/api/pgp/identity/envelope/recovery", "/api/auth/password"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		authRequestAs(srv, req, u.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var snapshot struct {
			Revision uint64                 `json:"pgpRevision"`
			Keyring  *users.PGPKeyringState `json:"keyring"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Revision != u.PGPRevision || !reflect.DeepEqual(snapshot.Keyring, u.PGPKeyring) {
			t.Fatalf("%s lost snapshot metadata: %s", path, rec.Body.String())
		}
	}
	tests := []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/api/pgp/identity/client", map[string]any{"publicKey": info.ArmoredPublicKey, "wrapped": `{"v":2}`, "source": "imported"}},
		{http.MethodPost, "/api/pgp/identity/rewrap", map[string]any{"wrapped": `{"v":2}`}},
		{http.MethodPut, "/api/pgp/identity/envelope/recovery", map[string]any{"envelope": `{"v":2}`}},
		{http.MethodPut, "/api/pgp/identity/envelope/device:test", map[string]any{"envelope": `{"v":2}`}},
		{http.MethodPost, "/api/auth/password", map[string]any{"oldAuthSecret": authSecret, "newAuthSecret": strings.Repeat("b", 64), "newLoginSalt": salt, "newIterations": 600000, "rewrappedPgpKey": `{"v":2}`}},
	}
	for _, test := range tests {
		test.body["expectedRevision"] = u.PGPRevision
		test.body["authSecret"] = authSecret
		body, err := json.Marshal(test.body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(test.method, test.path, bytes.NewReader(body))
		authRequestAs(srv, req, u.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"keyringUpgradeRequired":true`) {
			t.Fatalf("%s: %d %s", test.path, rec.Code, rec.Body.String())
		}
	}
	after, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PGPRevision != u.PGPRevision || after.PasswordHash != u.PasswordHash || after.PGPPrivateKeyWrapped != u.PGPPrivateKeyWrapped || !reflect.DeepEqual(after.PGPWrappedEnvelopes, u.PGPWrappedEnvelopes) {
		t.Fatal("rejected legacy requests changed data")
	}
	admin, err := srv.users.Create(context.Background(), "ring-admin", "admin-test-password", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/users/"+u.ID+"/reset-password", strings.NewReader(`{"password":"temporary-ring-password"}`))
	authRequestAs(srv, req, admin.ID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reset: %d %s", rec.Code, rec.Body.String())
	}
	after, err = srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.MustChangePassword || after.PGPPrivateKeyWrapped != u.PGPPrivateKeyWrapped || !reflect.DeepEqual(after.PGPKeyring, u.PGPKeyring) || after.PGPRevision != u.PGPRevision+1 {
		t.Fatal("admin reset did not preserve ring with a revision bump")
	}
}
