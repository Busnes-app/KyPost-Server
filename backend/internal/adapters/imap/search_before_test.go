package imap

import (
	"context"
	"strings"
	"testing"
)

// before pages a search: only matches below it are fetched, newest first.
func TestSearchMessagesPagesBelowBefore(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	var fetched []string
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		switch {
		case strings.HasPrefix(command, "UID SEARCH"):
			return "* SEARCH 3 5 9 12 20\r\n" + tag + " OK done\r\n", true
		case strings.HasPrefix(command, "UID FETCH"):
			server.mu.Lock()
			fetched = append(fetched, strings.Fields(command)[2])
			server.mu.Unlock()
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client("INBOX")
	for _, before := range []int{0, 12, 3} {
		if _, err := client.SearchMessages(context.Background(), "INBOX", "all", "x", 2, before); err != nil {
			t.Fatalf("before %d: %v", before, err)
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if want := []string{"12,20", "5,9"}; strings.Join(fetched, " ") != strings.Join(want, " ") {
		t.Fatalf("fetched %v, want %v", fetched, want)
	}
}
