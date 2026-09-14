package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busness-app/kypost-server/backend/internal/pgpmail"
)

// A client-custody draft is ciphertext the server appends verbatim. The
// plaintext fields of the same request are ignored: storing them would put
// the draft's text beside the ciphertext that exists to hide it.

func draftRequestBody(t *testing.T, fields map[string]any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

func encryptedDraftFor(t *testing.T, identity *pgpmail.Identity) string {
	t.Helper()
	plain := mailmsg.Message{
		From: "me@example.com", To: []string{"a@example.com"},
		Subject: "Plans", Body: "draft text", Mode: "plain",
	}.Build()
	encrypted, err := pgpmail.EncryptMIME(plain, []string{identity.ArmoredPublicKey}, nil)
	if err != nil {
		t.Fatalf("EncryptMIME: %v", err)
	}
	// The browser's wrapper carries a Date, which the shape check requires;
	// the server-side builder used here does not add one.
	return "Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" + string(encrypted)
}

func TestEncryptedDraftIsAppendedVerbatim(t *testing.T) {
	srv := newTestServer(t)
	userID, identity := testUserWithServerKey(t, srv)
	fake := &fakeMailClient{}
	draft := encryptedDraftFor(t, identity)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/mail/draft", draftRequestBody(t, map[string]any{
		"to": "a@example.com", "subject": pgpmail.OuterPlaceholderSubject, "body": "", "mode": "html",
		"pgpDraft": draft,
	}))
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: userID}))
	srv.serveDraftSave(rec, req, fake)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.savedDrafts) != 1 {
		t.Fatalf("saved %d drafts, want 1", len(fake.savedDrafts))
	}
	saved := fake.savedDrafts[0]
	if string(saved.Raw) != strings.TrimSpace(draft) {
		t.Fatal("the encrypted draft was not appended verbatim")
	}
	if saved.Body != "" || saved.Subject != pgpmail.OuterPlaceholderSubject {
		t.Fatalf("plaintext fields reached the draft: subject=%q body=%q", saved.Subject, saved.Body)
	}
}

func TestEncryptedDraftPlaintextFieldsAreIgnored(t *testing.T) {
	srv := newTestServer(t)
	userID, identity := testUserWithServerKey(t, srv)
	fake := &fakeMailClient{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/mail/draft", draftRequestBody(t, map[string]any{
		"to": "a@example.com", "cc": "cc@example.com", "bcc": "hidden@example.com",
		"subject": "the real subject", "body": "the real body", "mode": "html",
		"pgpDraft": encryptedDraftFor(t, identity),
	}))
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: userID}))
	srv.serveDraftSave(rec, req, fake)

	if rec.Code != 200 || len(fake.savedDrafts) != 1 {
		t.Fatalf("status = %d, saved=%d, body=%s", rec.Code, len(fake.savedDrafts), rec.Body.String())
	}
	if strings.Contains(string(fake.savedDrafts[0].Raw), "the real body") || fake.savedDrafts[0].Body != "" ||
		fake.savedDrafts[0].Subject == "the real subject" {
		t.Fatal("a plaintext field from an encrypted-draft request was stored")
	}
	// Cc and Bcc travel inside the ciphertext; the server is never handed them.
	if len(fake.savedDrafts[0].CC) != 0 || len(fake.savedDrafts[0].BCC) != 0 {
		t.Fatalf("cc/bcc from an encrypted-draft request reached the draft: cc=%v bcc=%v",
			fake.savedDrafts[0].CC, fake.savedDrafts[0].BCC)
	}
}

func TestEncryptedDraftRefusesEmptyFrom(t *testing.T) {
	srv := newTestServer(t)
	userID, identity := testUserWithServerKey(t, srv)
	fake := &fakeMailClient{}
	// The wrapper the browser emitted before it knew the account address.
	draft := strings.Replace(encryptedDraftFor(t, identity), "From: me@example.com", "From: ", 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/mail/draft", draftRequestBody(t, map[string]any{
		"to": "a@example.com", "subject": pgpmail.OuterPlaceholderSubject, "body": "", "mode": "html",
		"pgpDraft": draft,
	}))
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: userID}))
	srv.serveDraftSave(rec, req, fake)

	if rec.Code != 400 || len(fake.savedDrafts) != 0 {
		t.Fatalf("status = %d, saved=%d, want 400 and nothing saved; body=%s", rec.Code, len(fake.savedDrafts), rec.Body.String())
	}
}

func TestEncryptedDraftRefusesCleartext(t *testing.T) {
	srv := newTestServer(t)
	userID, _ := testUserWithServerKey(t, srv)
	fake := &fakeMailClient{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/mail/draft", draftRequestBody(t, map[string]any{
		"to": "a@example.com", "subject": "s", "body": "", "mode": "html",
		"pgpDraft": "From: me@example.com\r\nTo: a@example.com\r\nSubject: s\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\nContent-Type: text/plain\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nnot really\r\n",
	}))
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: userID}))
	srv.serveDraftSave(rec, req, fake)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.savedDrafts) != 0 {
		t.Fatal("a cleartext draft posted as pgpDraft was stored")
	}
}
