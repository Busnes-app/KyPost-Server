//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeMailRelayDirectModeAPI(t *testing.T) {
	s := newDirectoryTestServer(t)
	ctx := context.Background()
	const password = "direct-test-admin-password"
	admin, err := s.users.Create(ctx, "direct-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.users.Create(ctx, "direct-member", password, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(s, admin.ID)
	other, otherCSRF := mintSessionForTest(s, member.ID)
	_, err = s.nativeDomains.Configure(ctx, "example.test", s.ssoStore.Load().IssuerURL)
	if err != nil {
		t.Fatal(err)
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, e := s.nativeDomains.Read()
		return []string{d.RecordValue()}, e
	})
	body := map[string]any{"mode": "direct", "host": "mail.example.test", "authSecret": userAuthSecret(s, admin.ID, password)}
	call := func(method, token, csrf string) *httptest.ResponseRecorder {
		raw, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest(method, "/api/admin/mail-relay", strings.NewReader(string(raw)))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	if w := call("PUT", other, otherCSRF); w.Code != 403 {
		t.Fatal("member changed direct transport", w.Code)
	}
	if w := call("PUT", token, ""); w.Code != 403 {
		t.Fatal("direct transport bypassed CSRF", w.Code)
	}
	w := call("PUT", token, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var response struct {
		Mode, Transport, Generation string
		AuthRequired                bool
		Port                        int
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Mode != "direct" || response.Transport != "mx-starttls" || response.AuthRequired || response.Port != 0 {
		t.Fatal("direct status misreported", w.Body)
	}
	current, exists, err := mailmsg.ReadDomainRelay(filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil || !exists || current.Mode != "direct" {
		t.Fatal("direct profile not persisted", err)
	}
	body["smtpPassword"] = "must-not-be-stored"
	if w = call("PUT", token, csrf); w.Code != 409 {
		t.Fatal("mixed relay/direct profile accepted", w.Code)
	}
	unchanged, _, err := mailmsg.ReadDomainRelay(filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"))
	if err != nil || !unchanged.Equal(current) {
		t.Fatal("refusal changed configured transport", err)
	}
}

func TestNativeOutboundDirectSendReportsAllRecipientFailures(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "direct-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mailmsg.SaveDomainRelay(context.Background(), filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "mail.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	raw := mailmsg.Message{From: "one@example.test", To: []string{"first@invalid", "second@invalid"}, Subject: "direct refusal", Body: "retained"}.Build()
	ac := AuthContext{UserID: u.ID, NativeSendEpoch: u.NativeSendEpoch}
	req := httptest.NewRequest("POST", "/api/mail/send", nil)
	w := httptest.NewRecorder()
	s.finishNativeSend(w, req, ac, u, "one@example.test", []mailbox.OutboundDelivery{{Recipients: []string{"first@invalid", "second@invalid"}, Raw: raw}}, raw, false, nil, 0, "")
	var response struct {
		OK, SentSaved                        bool
		OutboxID                             string
		DeliveriesTotal, DeliveriesConfirmed int
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 503 || response.OK || response.SentSaved || response.DeliveriesTotal != 2 || response.DeliveriesConfirmed != 0 {
		t.Fatal("direct failure response hid recipient outcomes", w.Code, w.Body)
	}
	statuses, _, err := s.nativeOutbound().Status(req.Context(), u.ID, response.OutboxID)
	if err != nil || len(statuses) != 2 {
		t.Fatal("missing status", err)
	}
	for _, status := range statuses {
		if status.State != "failed" || status.Attempts != 1 {
			t.Fatal("recipient not attempted independently", statuses)
		}
	}
}
