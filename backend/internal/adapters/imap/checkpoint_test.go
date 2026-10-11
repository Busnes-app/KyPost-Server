package imap

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseCheckpoint(t *testing.T) {
	cases := []struct {
		in     string
		wantNS string
		uid    int
	}{
		{"", "", 0},
		{"500", "", 500},
		{" 500 ", "", 500},
		{"garbage", "", 0},
		{"7.abc:500", "7.abc", 500},
		{"7.abc:0", "7.abc", 0},
		{"7.abc:-1", "7.abc", 0},
	}
	for _, c := range cases {
		ns, uid := parseCheckpoint(c.in)
		if ns != c.wantNS || uid != c.uid {
			t.Errorf("parseCheckpoint(%q) = %q, %d; want %q, %d", c.in, ns, uid, c.wantNS, c.uid)
		}
	}
	for _, cp := range []string{"", "500", "7.abc:0", "7.abc:500"} {
		if got := formatCheckpoint(parseCheckpoint(cp)); got != cp {
			t.Errorf("format(parse(%q)) = %q", cp, got)
		}
	}
}

func TestUIDNamespaceReset(t *testing.T) {
	for _, c := range []struct{ prev, next, reset string }{
		{"A:500", "B:3", "B:0"},
		{"A:500", "A:600", ""},
		{"500", "B:3", ""}, // old format: keep the processed set
		{"", "B:3", ""},
		{"A:500", "600", ""},
	} {
		reset, changed := UIDNamespaceReset(c.prev, c.next)
		if reset != c.reset || changed != (c.reset != "") {
			t.Errorf("UIDNamespaceReset(%q, %q) = %q, %v; want %q", c.prev, c.next, reset, changed, c.reset)
		}
	}
}

func TestClampCheckpointKeepsNamespace(t *testing.T) {
	if got := ClampCheckpoint("A:10", "A:20", []string{"14"}); got != "A:13" {
		t.Fatalf("ClampCheckpoint = %q, want A:13", got)
	}
	// Legacy prev upgrades to next's namespace.
	if got := ClampCheckpoint("10", "A:20", []string{"14"}); got != "A:13" {
		t.Fatalf("ClampCheckpoint = %q, want A:13", got)
	}
	// ...also when the clamp holds the checkpoint at prev.
	if got := ClampCheckpoint("500", "A:501", []string{"501"}); got != "A:500" {
		t.Fatalf("ClampCheckpoint = %q, want A:500", got)
	}
	if got := ClampCheckpoint("500", "A:501", []string{"x"}); got != "A:500" {
		t.Fatalf("ClampCheckpoint = %q, want A:500", got)
	}
	// prev from another namespace is no floor for next's UIDs.
	if got := ClampCheckpoint("A:500", "B:20", []string{"14"}); got != "B:13" {
		t.Fatalf("ClampCheckpoint = %q, want B:13", got)
	}
	if got := ClampCheckpoint("A:500", "B:20", []string{"1"}); got != "B:0" {
		t.Fatalf("ClampCheckpoint = %q, want B:0", got)
	}
}

// TestListUnreadInboxUIDValidity drives the real adapter against a server whose
// UIDVALIDITY changes, with every unseen UID below the old checkpoint.
func TestListUnreadInboxUIDValidity(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", false, []fakeFolder{{name: "INBOX"}})
	var validity atomic.Int64
	validity.Store(7)
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		switch {
		case strings.HasPrefix(command, "STATUS"):
			return fmt.Sprintf("* STATUS INBOX (UIDVALIDITY %d)\r\n%s OK done\r\n", validity.Load(), tag), true
		case command == "UID SEARCH UNSEEN":
			return "* SEARCH\r\n" + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client("INBOX")
	ctx := context.Background()

	_, legacy, err := client.ListUnreadInbox(ctx, "500")
	if err != nil {
		t.Fatal(err)
	}
	ns7, uid := parseCheckpoint(legacy)
	if !strings.HasPrefix(ns7, "7.") || uid != 500 {
		t.Fatalf("old-format checkpoint upgraded to %q, want 7.<account>:500", legacy)
	}

	_, same, err := client.ListUnreadInbox(ctx, legacy)
	if err != nil || same != legacy {
		t.Fatalf("unchanged UIDVALIDITY: checkpoint %q, %v; want %q", same, err, legacy)
	}

	validity.Store(8)
	_, next, err := client.ListUnreadInbox(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	ns8, uid := parseCheckpoint(next)
	if !strings.HasPrefix(ns8, "8.") || uid != 0 {
		t.Fatalf("changed UIDVALIDITY: checkpoint %q, want 8.<account>:0", next)
	}
	if _, changed := UIDNamespaceReset(legacy, next); !changed {
		t.Fatal("UIDNamespaceReset did not report the UIDVALIDITY change")
	}

	other := server.client("INBOX")
	other.username = "someone-else@example.com"
	_, otherCP, err := other.ListUnreadInbox(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := UIDNamespaceReset(next, otherCP); !changed {
		t.Fatalf("another account kept namespace: %q -> %q", next, otherCP)
	}
}

// UIDVALIDITY is read from the STATUS attribute list, not from a mailbox name
// that happens to contain it.
func TestListUnreadInboxUIDValidityIgnoresMailboxName(t *testing.T) {
	quietRetries(t, 0)
	const folder = "UIDVALIDITY 42"
	server := newFakeIMAPServer(t, "/", false, []fakeFolder{{name: folder}})
	var validity atomic.Int64
	validity.Store(7)
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		switch {
		case strings.HasPrefix(command, "STATUS"):
			return fmt.Sprintf("* STATUS %q (UIDVALIDITY %d)\r\n%s OK done\r\n", folder, validity.Load(), tag), true
		case command == "UID SEARCH UNSEEN":
			return "* SEARCH\r\n" + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client(folder)
	ctx := context.Background()

	_, prev, err := client.ListUnreadInbox(ctx, "500")
	if err != nil {
		t.Fatal(err)
	}
	if ns, _ := parseCheckpoint(prev); !strings.HasPrefix(ns, "7.") {
		t.Fatalf("checkpoint %q, want namespace 7.<account>", prev)
	}
	validity.Store(8)
	_, next, err := client.ListUnreadInbox(ctx, prev)
	if err != nil {
		t.Fatal(err)
	}
	if ns, uid := parseCheckpoint(next); !strings.HasPrefix(ns, "8.") || uid != 0 {
		t.Fatalf("changed UIDVALIDITY: checkpoint %q, want 8.<account>:0", next)
	}
}

// A server that omits UIDVALIDITY keeps the old bare-UID behaviour.
func TestListUnreadInboxWithoutUIDValidity(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", false, []fakeFolder{{name: "INBOX"}})
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		if command == "UID SEARCH UNSEEN" {
			return "* SEARCH 3\r\n" + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	_, next, err := server.client("INBOX").ListUnreadInbox(context.Background(), "500")
	if err != nil || next != "500" {
		t.Fatalf("checkpoint %q, %v; want 500", next, err)
	}
}
