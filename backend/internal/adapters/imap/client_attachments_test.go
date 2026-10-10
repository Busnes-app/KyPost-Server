package imap

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	goimap "github.com/BrianLeishman/go-imap"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// TestFetchAttachmentsOversizedSearchCriteria pins the exact SEARCH
// fetchAttachments composes: the single-UID counterpart to
// GetMessageBodies's "UID <set> LARGER <cap>". IMAP ANDs search keys, so this
// asks the server one question about one message — "is the message at this UID
// bigger than we will hold in memory?" — and gets it answered from the
// server's own RFC822.SIZE, without a byte of the body crossing the wire.
func TestFetchAttachmentsOversizedSearchCriteria(t *testing.T) {
	withLoweredMaxInboundMessageBytes(t, 100)
	sb := goimap.Search().UID(strconv.Itoa(7)).Larger(int(mailmsg.MaxInboundMessageBytes))
	if got, want := sb.Build(), "UID 7 LARGER 100"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFetchAttachmentsRefusesOversizeBeforeFetching is the property the old
// post-fetch check got wrong: an oversized message must be refused from the
// server's own RFC822.SIZE (a UID LARGER search) before a byte of it is
// requested, because opening a message loads its attachment list and how big
// that message is belongs to whoever sent it.
func TestFetchAttachmentsRefusesOversizeBeforeFetching(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		if strings.HasPrefix(command, "UID SEARCH") && strings.Contains(command, "LARGER") {
			return "* SEARCH 7\r\n" + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client("INBOX")

	if _, err := client.ListAttachments(context.Background(), "INBOX", 7); !errors.Is(err, mailmsg.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	for _, c := range server.commandsMatching("UID") {
		if strings.HasPrefix(c, "UID FETCH") {
			t.Fatalf("oversized message was fetched: %q", c)
		}
	}
}
