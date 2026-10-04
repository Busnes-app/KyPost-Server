package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeRecoveryEvidenceCannotApplyDirectoryState(t *testing.T) {
	for _, shape := range []string{"snapshot", "SCIM user"} {
		t.Run(shape, func(t *testing.T) {
			srv := newDirectoryTestServer(t)
			const subject = "restored-subject"
			directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "before-restore", 1, scimUser(subject, "restored_user", true)))
			hold := filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile)
			if err := os.WriteFile(hold, []byte(`{"version":1,"epoch":"held-for-reconciliation"}`), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			nonce := strings.Repeat("a", 64)
			var payload any = map[string]any{
				"version": 1, "issuer": "https://idp.example", "systemId": "paired-kypost", "nonce": nonce,
				"issuedAt": now, "expiresAt": now.Add(5 * time.Minute),
				"subjects": []any{map[string]any{"id": subject, "revision": 2, "profile": map[string]any{"id": subject, "externalId": subject, "active": false, "roles": []string{}}}},
			}
			if shape == "SCIM user" {
				user := scimUser(subject, "restored_user", true, sso.AdminAppRole)
				user["meta"] = map[string]any{"version": `W/"2"`}
				payload = user
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := syncauth.Sign([]byte(testSyncKey), now, "recovery.evidence", nonce, body)
			if err != nil {
				t.Fatal(err)
			}
			// Authentication succeeds; refusal must come from the purpose/resource boundary.
			if _, err := syncauth.Verify([]byte(testSyncKey), signed, body, syncauth.Options{}); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(srv.configDir, "users.json"), filepath.Join(srv.configDir, "sso-lifecycle.json"), hold}
			before := make([][]byte, len(paths))
			for i, path := range paths {
				before[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/api/sync/webhook", bytes.NewReader(body))
			signed.Apply(req)
			rec := httptest.NewRecorder()
			srv.handleSyncWebhook(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("recovery evidence status=%d: %s", rec.Code, rec.Body.String())
			}
			for i, path := range paths {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before[i], after) {
					t.Fatalf("refused recovery evidence changed %s: %v", filepath.Base(path), err)
				}
			}
			// Refusal must not consume a directory event ID or prevent ordinary sync.
			directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", nonce, 2, scimUser(subject, "restored_user", true, sso.AdminAppRole)))
			user, err := srv.users.GetBySSOSub(subject)
			if err != nil || user.Role != users.RoleAdmin || sso.RequireNativeRestoreReleased(srv.stateDir) == nil {
				t.Fatalf("ordinary sync/retained hold: user=%+v err=%v", user, err)
			}
		})
	}
}
