package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

// before= is untrusted input that becomes an IMAP UID range. Anything that is
// not a canonical positive message reference is refused before the adapter
// is called; a valid one pages through the adapter and hands back the cursor
// for the next page.
func TestServeInboxBeforePagesOlderMail(t *testing.T) {
	srv := newTestServer(t)
	all, _ := srv.users.List()
	userID := all[0].ID

	for _, bad := range []string{"abc", "0", "-3", "05", "+5", "9999999999999999999999", "5 6"} {
		fake := &fakeMailClient{}
		rec := httptest.NewRecorder()
		srv.serveInboxBefore(rec, context.Background(), userID, fake, "INBOX", bad, 50)
		if rec.Code != 400 || len(fake.beforeCalls) != 0 {
			t.Fatalf("before=%q: status %d, adapter calls %v; want 400 and no call", bad, rec.Code, fake.beforeCalls)
		}
	}

	fake := &fakeMailClient{
		olderOverviews: []imapadapter.Overview{
			{UID: 41, MessageID: "41", Subject: "newer", Sender: "a@example.com", Status: "read", AtUTC: "2026-01-02T00:00:00Z"},
			{UID: 40, MessageID: "40", Subject: "older", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"},
		},
		olderHasMore: true,
	}
	rec := httptest.NewRecorder()
	srv.serveInboxBefore(rec, context.Background(), userID, fake, "INBOX", "42", 2)
	if rec.Code != 200 || len(fake.beforeCalls) != 1 || fake.beforeCalls[0] != 42 {
		t.Fatalf("status %d, adapter calls %v; body=%s", rec.Code, fake.beforeCalls, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"hasMore":true`) || !strings.Contains(body, `"nextBefore":"40"`) {
		t.Fatalf("missing paging fields: %s", body)
	}
	if got := allEmails(decodeInboxResponse(t, rec)); len(got) != 2 || strings.Contains(body, `"cursor"`) {
		t.Fatalf("page must carry both rows and no cursor: %s", body)
	}

	fake = &fakeMailClient{}
	rec = httptest.NewRecorder()
	srv.serveInboxBefore(rec, context.Background(), userID, fake, "INBOX", "1", 2)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hasMore":false`) || strings.Contains(rec.Body.String(), "nextBefore") {
		t.Fatalf("empty last page: %d %s", rec.Code, rec.Body.String())
	}
}
