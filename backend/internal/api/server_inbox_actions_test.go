package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
)

// TestInboxActionsLabelAndUnlabel proves POST /api/inbox/actions'
// "label"/"unlabel" cases call ApplyLabel/RemoveLabel directly (bypassing
// ApplyInboxAction) and reject a missing keyword with 400.
func TestInboxActionsLabelAndUnlabel(t *testing.T) {
	srv := newTestServer(t)
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	all, _ := srv.users.List()
	userID := all[0].ID

	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw",
		Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatalf("writeIMAPConfigPayload: %v", err)
	}
	fake := &fakeMailClient{}
	srv.userMu.Lock()
	srv.userMail[userID] = &serverMailEntry{client: fake, updatedAt: "test"}
	srv.userMu.Unlock()

	labelBody, _ := json.Marshal(map[string]any{
		"action":     "label",
		"messageIds": []string{"1"},
		"keyword":    "VIP",
	})
	labelReq := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", bytes.NewReader(labelBody))
	authRequest(srv, labelReq)
	labelRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(labelRec, labelReq)
	if labelRec.Code != http.StatusOK {
		t.Fatalf("label status = %d, body=%s", labelRec.Code, labelRec.Body.String())
	}
	if len(fake.appliedLabels) != 1 || fake.appliedLabels[0].messageID != "1" || fake.appliedLabels[0].label != "VIP" {
		t.Fatalf("expected ApplyLabel(1, VIP), got %+v", fake.appliedLabels)
	}

	unlabelBody, _ := json.Marshal(map[string]any{
		"action":     "unlabel",
		"messageIds": []string{"1"},
		"keyword":    "VIP",
	})
	unlabelReq := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", bytes.NewReader(unlabelBody))
	authRequest(srv, unlabelReq)
	unlabelRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(unlabelRec, unlabelReq)
	if unlabelRec.Code != http.StatusOK {
		t.Fatalf("unlabel status = %d, body=%s", unlabelRec.Code, unlabelRec.Body.String())
	}
	if len(fake.removedLabels) != 1 || fake.removedLabels[0].messageID != "1" || fake.removedLabels[0].label != "VIP" {
		t.Fatalf("expected RemoveLabel(1, VIP), got %+v", fake.removedLabels)
	}

	missingKeywordBody, _ := json.Marshal(map[string]any{
		"action":     "label",
		"messageIds": []string{"1"},
	})
	missingReq := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", bytes.NewReader(missingKeywordBody))
	authRequest(srv, missingReq)
	missingRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing keyword, got %d", missingRec.Code)
	}
}

// folderLabelClient is an IMAP-shaped client that labels per folder.
type folderLabelClient struct {
	*fakeMailClient
	labelled []string
}

func (f *folderLabelClient) LabelIn(_ context.Context, mailbox, id, label string, add bool) error {
	f.labelled = append(f.labelled, fmt.Sprintf("%s/%s/%s/%v", mailbox, id, label, add))
	return nil
}

// The folder the message was listed in reaches the label write.
func TestInboxActionsLabelUsesTheMessagesFolder(t *testing.T) {
	srv := newTestServer(t)
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	all, _ := srv.users.List()
	userID := all[0].ID
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.com", Port: 993, Username: "alice@example.com", Password: "pw", Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	fake := &folderLabelClient{fakeMailClient: &fakeMailClient{}}
	srv.userMu.Lock()
	srv.userMail[userID] = &serverMailEntry{client: fake, updatedAt: "test"}
	srv.userMu.Unlock()
	for _, action := range []string{"label", "unlabel"} {
		body, _ := json.Marshal(map[string]any{"action": action, "messageIds": []string{"42"}, "keyword": "VIP", "mailbox": "Archive"})
		req := httptest.NewRequest(http.MethodPost, "/api/inbox/actions", bytes.NewReader(body))
		authRequest(srv, req)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
	}
	if want := []string{"Archive/42/VIP/true", "Archive/42/VIP/false"}; !slices.Equal(fake.labelled, want) || len(fake.appliedLabels)+len(fake.removedLabels) != 0 {
		t.Fatalf("labelled %v (default-folder calls %d), want %v", fake.labelled, len(fake.appliedLabels)+len(fake.removedLabels), want)
	}
}
