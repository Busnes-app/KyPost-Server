package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

func TestIncomingPreferenceRequiresCredentialAndAcknowledgement(t *testing.T) {
	server := newTestServer(t)
	id := server.mustBootstrapUserID(t)
	password := stepUpPassword(t, server, id)
	identity, err := pgpmail.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	seedClientIdentity(t, server, id, identity.ArmoredPublicKey)
	user, err := server.users.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := server.userMailCacheStore(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Upsert("INBOX", []mailcache.Entry{{UID: 7, MessageID: "7", Subject: "secret", Body: "plaintext"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		body   map[string]any
		status int
	}{
		{"missing credential", map[string]any{"enabled": true, "acknowledgeReplacement": true, "expectedRevision": user.PGPRevision}, http.StatusForbidden},
		{"missing acknowledgement", map[string]any{"enabled": true, "password": password, "expectedRevision": user.PGPRevision}, http.StatusBadRequest},
		{"stale revision", map[string]any{"enabled": true, "password": password, "acknowledgeReplacement": true, "expectedRevision": user.PGPRevision + 1}, http.StatusConflict},
		{"enable", map[string]any{"enabled": true, "password": password, "acknowledgeReplacement": true, "expectedRevision": user.PGPRevision}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.body)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/pgp/incoming", bytes.NewReader(body))
			authRequest(server, req)
			rec := httptest.NewRecorder()
			server.withAuth(server.handlePGPIncoming)(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status %d want %d: %s", rec.Code, test.status, rec.Body.String())
			}
		})
	}
	settings, err := config.LoadUserSettings(server.userSettingsPath(id))
	if err != nil || !settings.EncryptIncoming {
		t.Fatalf("enabled=%v err=%v", settings.EncryptIncoming, err)
	}
	entries, _, err := cache.Snapshot("INBOX", 1)
	if err != nil || len(entries) != 1 || entries[0].Body != "" {
		t.Fatalf("plaintext cache persisted: %v %v", entries, err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/pgp/incoming", bytes.NewBufferString(`{"enabled":false,"password":"`+password+`"}`))
	authRequest(server, req)
	rec := httptest.NewRecorder()
	server.withAuth(server.handlePGPIncoming)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %s", rec.Body.String())
	}
	settings, err = config.LoadUserSettings(server.userSettingsPath(id))
	if err != nil || settings.EncryptIncoming {
		t.Fatalf("disable failed: %v %v", settings, err)
	}
	if err := cache.Upsert("INBOX", []mailcache.Entry{{UID: 7, MessageID: "7", Body: "plaintext"}}); err != nil {
		t.Fatal(err)
	}
	entries, _, err = cache.Snapshot("INBOX", 1)
	if err != nil || entries[0].Body != "" {
		t.Fatal("disabling reintroduced plaintext cache")
	}
}
