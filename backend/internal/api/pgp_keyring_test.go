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

	// Matching-ring restore opts in explicitly; it changes no credential/recovery data.
	nearCapacity, err := json.Marshal(map[string]any{"v": 2, "data": strings.Repeat("x", users.MaxWrappedEnvelopeBytes-32)})
	if err != nil {
		t.Fatal(err)
	}
	restoreBody := map[string]any{"authSecret": authSecret, "wrapped": string(nearCapacity), "expectedFingerprint": u.PGPFingerprint, "expectedRevision": u.PGPRevision, "keyringVersion": 2}
	restore := func() *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(restoreBody)
		req := httptest.NewRequest(http.MethodPost, "/api/pgp/identity/rewrap", bytes.NewReader(encoded))
		authRequestAs(srv, req, u.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := restore(); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown restore version: %d", rec.Code)
	}
	restoreBody["keyringVersion"] = 1
	restoreBody["authSecret"] = strings.Repeat("c", 64)
	if rec := restore(); rec.Code != http.StatusUnauthorized {
		t.Fatalf("restore step-up: %d", rec.Code)
	}
	restoreBody["authSecret"] = authSecret
	restoreBody["expectedRevision"] = u.PGPRevision + 1
	if rec := restore(); rec.Code != http.StatusConflict {
		t.Fatalf("future restore revision: %d", rec.Code)
	}
	restoreBody["expectedRevision"] = u.PGPRevision
	if rec := restore(); rec.Code != http.StatusOK {
		t.Fatalf("ring restore: %d %s", rec.Code, rec.Body.String())
	}
	restored, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.PGPPrivateKeyWrapped != restoreBody["wrapped"] || restored.PasswordHash != u.PasswordHash || restored.PGPRevision != u.PGPRevision+1 ||
		!reflect.DeepEqual(restored.PGPKeyring, u.PGPKeyring) || !reflect.DeepEqual(restored.PGPWrappedEnvelopes, u.PGPWrappedEnvelopes) {
		t.Fatal("restore altered retained material")
	}
	u = restored

	recoveryBody := map[string]any{"authSecret": authSecret, "envelope": string(nearCapacity), "expectedFingerprint": u.PGPFingerprint, "expectedRevision": u.PGPRevision, "keyringVersion": 1}
	putRecovery := func(slot string) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(recoveryBody)
		req := httptest.NewRequest(http.MethodPut, "/api/pgp/identity/envelope/"+slot, bytes.NewReader(encoded))
		authRequestAs(srv, req, u.ID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := putRecovery("device:test"); rec.Code != http.StatusBadRequest {
		t.Fatalf("device opt-in: %d", rec.Code)
	}
	recoveryBody["keyringVersion"] = 2
	if rec := putRecovery("recovery"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown version: %d", rec.Code)
	}
	recoveryBody["keyringVersion"] = 1
	recoveryBody["expectedRevision"] = u.PGPRevision + 1
	if rec := putRecovery("recovery"); rec.Code != http.StatusConflict {
		t.Fatalf("future revision: %d", rec.Code)
	}
	recoveryBody["expectedRevision"] = u.PGPRevision
	unchanged, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged, u) {
		t.Fatal("rejected slot write changed data")
	}
	if rec := putRecovery("recovery"); rec.Code != http.StatusOK {
		t.Fatalf("ring recovery: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PGPRevision != u.PGPRevision+1 || saved.PasswordHash != u.PasswordHash || saved.PGPPrivateKeyWrapped != u.PGPPrivateKeyWrapped || !reflect.DeepEqual(saved.PGPKeyring, u.PGPKeyring) || saved.PGPWrappedEnvelopes[0].Envelope != string(nearCapacity) {
		t.Fatal("recovery altered current ring")
	}
	u = saved
	// Version opt-in is explicit, and the revision belongs to the verified credential snapshot.
	body := map[string]any{"oldAuthSecret": authSecret, "newAuthSecret": authSecret, "newLoginSalt": salt,
		"newIterations": 600000, "rewrappedPgpKey": `{"v":2,"password":"rewrapped-ring"}`, "keyringVersion": 1}
	for _, version := range []int{0, 2, 1} {
		body["keyringVersion"] = version
		if version != 1 {
			body["expectedRevision"] = u.PGPRevision
		} else {
			delete(body, "expectedRevision")
		}
		rec := changePasswordAs(t, srv, u.ID, body)
		if rec.Code == http.StatusOK {
			t.Fatal("unknown version or missing revision accepted")
		}
	}
	body["expectedRevision"] = u.PGPRevision + 1
	if rec := changePasswordAs(t, srv, u.ID, body); rec.Code != http.StatusConflict {
		t.Fatalf("future revision: %d", rec.Code)
	}
	body["expectedRevision"] = u.PGPRevision
	if rec := changePasswordAs(t, srv, u.ID, body); rec.Code != http.StatusOK {
		t.Fatalf("ring password: %d %s", rec.Code, rec.Body.String())
	}
	changed, err := srv.users.Get(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.PGPRevision != u.PGPRevision+1 || changed.PGPPrivateKeyWrapped != body["rewrappedPgpKey"] ||
		!reflect.DeepEqual(changed.PGPKeyring, u.PGPKeyring) || !reflect.DeepEqual(changed.PGPWrappedEnvelopes, u.PGPWrappedEnvelopes) {
		t.Fatal("ring password transaction lost material")
	}
	u = changed
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
