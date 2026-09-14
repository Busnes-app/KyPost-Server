package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

func TestPGPRevisionHTTPRejectsSameKeyStaleWrites(t *testing.T) {
	srv := newTestServer(t)
	u := clientProtectedUser(t, srv)
	password := stepUpPassword(t, srv, u.ID)
	before, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := srv.users.RewrapPGPPrivateKey(u.ID, `{"v":2,"new":true}`, before.PGPFingerprint, &before.PGPRevision)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pgpmail.GenerateIdentity("Revision test", "revision@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, path, slot string
		handler                  http.HandlerFunc
		payload                  map[string]any
	}{
		{"identity", http.MethodPost, "/api/pgp/identity/client", "", srv.handlePGPIdentityClient, map[string]any{"publicKey": identity.ArmoredPublicKey, "wrapped": `{"v":2}`, "source": "imported"}},
		{"rewrap", http.MethodPost, "/api/pgp/identity/rewrap", "", srv.handlePGPRewrapKey, map[string]any{"wrapped": `{"v":2}`, "expectedFingerprint": before.PGPFingerprint}},
		{"recovery", http.MethodPut, "/api/pgp/identity/envelope/recovery", "recovery", srv.handlePGPPutEnvelopeSlot, map[string]any{"envelope": `{"v":2}`, "expectedFingerprint": before.PGPFingerprint}},
		{"device", http.MethodPut, "/api/pgp/identity/envelope/device:test", "device:test", srv.handlePGPPutEnvelopeSlot, map[string]any{"envelope": `{"v":2}`}},
		{"slot-delete", http.MethodDelete, "/api/pgp/identity/envelope/recovery", "recovery", srv.handlePGPDeleteEnvelopeSlot, map[string]any{}},
		{"identity-delete", http.MethodDelete, "/api/pgp/identity", "", srv.handlePGPIdentity, map[string]any{}},
		{"password-pair", http.MethodPost, "/api/auth/password", "", srv.handleChangePassword, map[string]any{"oldPassword": password, "newAuthSecret": strings.Repeat("a", 64), "newLoginSalt": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), "newIterations": 600000, "rewrappedPgpKey": `{"v":2}`}},
		{"legacy-password", http.MethodPost, "/api/auth/password", "", srv.handleChangePassword, map[string]any{"oldPassword": password, "newPassword": "replacement-test-password"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.payload["expectedRevision"] = before.PGPRevision
			test.payload["password"] = password
			handler := srv.withAuth(func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("slot", test.slot); test.handler(w, r) })
			rec := doJSONAuth(srv, handler, test.method, test.path, test.payload, u.ID)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"pgpStateChanged":true`) {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			after, err := srv.users.Get(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.PGPRevision != current.PGPRevision || after.PasswordHash != current.PasswordHash || after.PGPPrivateKeyWrapped != current.PGPPrivateKeyWrapped || after.PGPPublicKey != current.PGPPublicKey || len(after.PGPWrappedEnvelopes) != len(current.PGPWrappedEnvelopes) {
				t.Fatal("rejected write changed credentials or PGP material")
			}
		})
	}
}

func TestPGPRevisionHTTPMetadataAndCompatibility(t *testing.T) {
	srv := newTestServer(t)
	u := clientProtectedUser(t, srv)
	password := stepUpPassword(t, srv, u.ID)
	before, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, slot string, payload any, handler http.HandlerFunc) *httptest.ResponseRecorder {
		return doJSONAuth(srv, srv.withAuth(func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("slot", slot); handler(w, r) }), method, path, payload, u.ID)
	}
	// Legacy writers can omit the guard until account conversion ships.
	rec := call(http.MethodPut, "/api/pgp/identity/envelope/recovery", "recovery", map[string]any{"password": password, "envelope": `{"v":2,"recovery":true}`}, srv.handlePGPPutEnvelopeSlot)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy put: %d %s", rec.Code, rec.Body.String())
	}
	var committed struct {
		PGPRevision uint64 `json:"pgpRevision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.PGPRevision != before.PGPRevision+1 {
		t.Fatal("write omitted committed revision")
	}
	for _, test := range []struct {
		path, slot string
		handler    http.HandlerFunc
	}{
		{"/api/pgp/bootstrap", "", srv.handlePGPBootstrap},
		{"/api/pgp/identity", "", srv.handlePGPIdentity},
		{"/api/pgp/identity/wrapped", "", srv.handlePGPWrappedKey},
		{"/api/pgp/identity/envelope/recovery", "recovery", srv.handlePGPGetEnvelopeSlot},
	} {
		rec := call(http.MethodGet, test.path, test.slot, nil, test.handler)
		var got struct {
			PGPRevision uint64 `json:"pgpRevision"`
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", test.path, rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.PGPRevision != committed.PGPRevision {
			t.Fatalf("%s: inconsistent snapshot revision", test.path)
		}
	}
	for _, expected := range []any{0, -1, 1.5, "bad", users.MaxPGPRevision + 1} {
		rec := call(http.MethodPost, "/api/pgp/identity/rewrap", "", map[string]any{"password": password, "wrapped": `{"v":2}`, "expectedRevision": expected}, srv.handlePGPRewrapKey)
		if rec.Code != http.StatusConflict && rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid/stale expectation %v: %d", expected, rec.Code)
		}
	}
}

func TestPGPRevisionHTTPPasswordPairCommitsOneRevision(t *testing.T) {
	srv := newTestServer(t)
	user := clientProtectedUser(t, srv)
	password := stepUpPassword(t, srv, user.ID)
	before, err := srv.users.Get(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := changePasswordAs(t, srv, user.ID, map[string]any{
		"oldPassword":      password,
		"newAuthSecret":    strings.Repeat("b", 64),
		"newLoginSalt":     base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
		"newIterations":    600000,
		"rewrappedPgpKey":  `{"v":2,"paired":true}`,
		"expectedRevision": before.PGPRevision,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password pair: %d %s", rec.Code, rec.Body.String())
	}
	after, err := srv.users.Get(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		PGPRevision uint64 `json:"pgpRevision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if after.PGPRevision != before.PGPRevision+1 || result.PGPRevision != after.PGPRevision ||
		after.PasswordHash == before.PasswordHash || after.PGPPrivateKeyWrapped != `{"v":2,"paired":true}` {
		t.Fatal("credential/envelope/revision did not commit together")
	}
}
