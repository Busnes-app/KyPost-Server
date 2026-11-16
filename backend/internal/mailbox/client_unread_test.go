package mailbox

import (
	"context"
	"strconv"
	"testing"
)

func TestNativeClientUnreadAction(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	uid := importClient(t, s, "u", []byte("From: s@outside.test\r\nSubject: x\r\n\r\nbody"))
	status := func() string {
		t.Helper()
		o, err := c.ListOverviews(ctx, "INBOX", 10)
		must(t, err)
		return o[0].Status
	}
	must(t, c.ApplyInboxAction(ctx, strconv.Itoa(uid), "read", "INBOX", ""))
	if status() != "read" {
		t.Fatal("read did not stick")
	}
	must(t, c.ApplyInboxAction(ctx, strconv.Itoa(uid), "unread", "INBOX", ""))
	if got := status(); got != "unread" {
		t.Fatalf("status after unread = %q", got)
	}
}

// Flag and unflag set the native \Flagged (starred) state; a missing message is ErrNotFound.
func TestNativeClientFlagAction(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	uid := importClient(t, s, "u", []byte("From: s@outside.test\r\nSubject: x\r\n\r\nbody"))
	starred := func() bool {
		t.Helper()
		m, err := s.metadata(ctx, "INBOX", int64(uid))
		must(t, err)
		return m.Starred
	}
	must(t, c.ApplyInboxAction(ctx, strconv.Itoa(uid), "flag", "INBOX", ""))
	must(t, c.ApplyInboxAction(ctx, strconv.Itoa(uid), "flag", "INBOX", ""))
	if !starred() {
		t.Fatal("flag did not stick")
	}
	must(t, c.ApplyInboxAction(ctx, strconv.Itoa(uid), "unflag", "INBOX", ""))
	if starred() {
		t.Fatal("unflag did not stick")
	}
	if err := c.ApplyInboxAction(ctx, strconv.Itoa(uid+1), "flag", "INBOX", ""); err != ErrNotFound {
		t.Fatalf("missing message: %v", err)
	}
}
