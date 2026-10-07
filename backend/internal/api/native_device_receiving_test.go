//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// Join real native allocation, pairing/register, durable ingress import and
// device API selection in the actual TLS sending fixture. SMTP/app receiving
// admission is qualified separately; this uses the trusted-local store bridge.
func qualifyNativeDeviceReceiving(t *testing.T, s *Server, userID string, sessionRequest func(string, string, []byte) *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if _, err := os.Stat(s.userIMAPConfigPath(userID)); !os.IsNotExist(err) {
		t.Fatal("native device test must have no IMAP configuration", err)
	}
	pairing := sessionRequest("GET", "/api/notifications/pairing", nil)
	var proof struct {
		Configured   bool   `json:"configured"`
		SubscriberID string `json:"subscriberId"`
		Token        string `json:"pairingToken"`
	}
	if pairing.Code != 200 || json.Unmarshal(pairing.Body.Bytes(), &proof) != nil || !proof.Configured || proof.Token == "" {
		t.Fatal("native pairing failed", pairing.Code)
	}
	payload, err := json.Marshal(map[string]string{"subscriberId": proof.SubscriberID, "pairingToken": proof.Token, "deviceId": "native-roundtrip-device", "deviceToken": "synthetic-fcm-token", "platform": "android"})
	if err != nil {
		t.Fatal(err)
	}
	register := httptest.NewRecorder()
	s.routes().ServeHTTP(register, httptest.NewRequest("POST", "/api/notifications/native/register", bytes.NewReader(payload)))
	var device struct {
		ID     string `json:"deviceId"`
		Secret string `json:"deviceSecret"`
	}
	if register.Code != 200 || json.Unmarshal(register.Body.Bytes(), &device) != nil || device.ID != "native-roundtrip-device" || device.Secret == "" {
		t.Fatal("native registration failed", register.Code)
	}
	unauthorized := httptest.NewRecorder()
	wrongSecret := httptest.NewRequest("GET", "/api/inbox?mailbox=INBOX", nil)
	setDeviceHeaders(wrongSecret, device.ID, "wrong-device-secret")
	s.routes().ServeHTTP(unauthorized, wrongSecret)
	if unauthorized.Code != 401 {
		t.Fatal("device inbox accepted a wrong secret", unauthorized.Code)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		setDeviceHeaders(r, device.ID, device.Secret)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("device %s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	ctx := context.Background()
	a, _, err := s.nativeMailAssignment(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	limits := ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 20}
	root := filepath.Join(t.TempDir(), "holding")
	holding, err := ingress.Open(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if holding != nil {
			_ = holding.Close()
		}
	})
	route := ingress.Route{Address: a.Address, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Generation: a.Revision, Active: true, ValidUntil: time.Now().Add(time.Minute)}
	if err = holding.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	attachment := []byte("native attachment\x00exact bytes")
	raw := mailmsg.Message{From: "visible@outside.test", To: []string{a.Address}, Subject: "incoming native roundtrip", Body: "incoming body <visible@outside.test>", Attachments: []mailmsg.Attachment{{Name: "file.bin", MimeType: "application/octet-stream", Content: attachment}}}.Build()
	if err = holding.Bind(ctx, "qualified-test", "device-incoming", "visible@outside.test", a.Address); err != nil {
		t.Fatal(err)
	}
	if err = holding.Accept(ctx, "qualified-test", "device-incoming", "visible@outside.test", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err = holding.Close(); err != nil {
		t.Fatal(err)
	}
	holding, err = ingress.OpenExisting(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(userID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })
	if err = holding.Import(ctx, "qualified-test", "device-incoming", func(owner mailbox.Owner) (*mailbox.Store, error) {
		if owner != a.Owner {
			return nil, mailbox.ErrOwner
		}
		return box, nil
	}); err != nil {
		t.Fatal(err)
	}
	delivery, err := holding.Get(ctx, "qualified-test", "device-incoming")
	if err != nil || delivery.State != "archived" || len(delivery.Raw) != 0 {
		t.Fatal("import did not retain receipt and release holding payload", err)
	}
	stored, err := box.List(ctx, "INBOX", 0, 10)
	if err != nil || len(stored) != 1 {
		t.Fatal("native import missing or duplicated", err)
	}
	storedRaw, err := box.Raw(ctx, "INBOX", stored[0].ID)
	if err != nil || !bytes.Equal(storedRaw, raw) {
		t.Fatal("native import rewrote MIME", err)
	}
	inbox := decodeInboxResponse(t, request("GET", "/api/inbox?mailbox=INBOX&since=999999", ""))
	emails := allEmails(inbox)
	if len(emails) != 1 || inbox.Delta || inbox.Cursor != 0 || emails[0].Subject != "incoming native roundtrip" || emails[0].Body != "incoming body <visible@outside.test>" || emails[0].BodyMode != "plain" {
		t.Fatal("native device inbox lost imported mail/full snapshot contract")
	}
	query := "?mailbox=INBOX&messageId=" + emails[0].MessageID
	if body := request("GET", "/api/mail/body"+query, ""); !bytes.Contains(body.Body.Bytes(), []byte("incoming body")) {
		t.Fatal("device body missing")
	}
	if download := request("GET", "/api/mail/attachment"+query+"&index=0", ""); !bytes.Equal(download.Body.Bytes(), attachment) {
		t.Fatal("device attachment bytes changed")
	}
	request("POST", "/api/inbox/actions", fmt.Sprintf(`{"action":"label","keyword":"Travel","mailbox":"INBOX","messageIds":[%q]}`, emails[0].MessageID))
	labeled := allEmails(decodeInboxResponse(t, request("GET", "/api/inbox?mailbox=INBOX&since=999999", "")))
	if len(labeled) != 1 || !slices.Contains(labeled[0].Keywords, "Travel") {
		t.Fatal("device label did not persist")
	}
	if _, err := os.Stat(s.userIMAPConfigPath(userID)); !os.IsNotExist(err) {
		t.Fatal("native device path introduced an IMAP configuration", err)
	}
	return device.ID, device.Secret
}
