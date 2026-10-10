package imap

import (
	"context"
	"strings"
	"testing"
)

// Older-mail paging must ask the server for the UID range below the cursor,
// fetch envelopes only for the newest `limit` of it, and never trust a UID the
// server returns outside the range.
func TestListOverviewsBeforeSearchesBelowTheCursor(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		if strings.HasPrefix(command, "UID SEARCH") {
			// 12 is outside the requested range and must be dropped.
			return "* SEARCH 3 5 7 8 12\r\n" + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client("INBOX")

	_, more, err := client.ListOverviewsBefore(context.Background(), "INBOX", 10, 2)
	if err != nil {
		t.Fatalf("ListOverviewsBefore: %v", err)
	}
	if !more {
		t.Fatal("hasMore = false with 3 and 5 still below the page")
	}
	var search, fetch string
	for _, c := range server.commandsMatching("UID") {
		if strings.HasPrefix(c, "UID SEARCH") {
			search = c
		}
		if strings.HasPrefix(c, "UID FETCH") {
			fetch = c
		}
	}
	if search != "UID SEARCH UID 1:9" {
		t.Fatalf("search = %q, want the range strictly below the cursor", search)
	}
	if !strings.HasPrefix(fetch, "UID FETCH 7,8 ") {
		t.Fatalf("fetch = %q, want the newest two UIDs below the cursor only", fetch)
	}

	if page, more, err := client.ListOverviewsBefore(context.Background(), "INBOX", 1, 2); err != nil || more || len(page) != 0 {
		t.Fatalf("before=1 = %+v %v %v, want an empty last page", page, more, err)
	}
}
