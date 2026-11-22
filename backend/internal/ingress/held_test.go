package ingress

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// HeldBytesByMailbox agrees with HeldBytes for every mailbox, counts a delivery bound to
// two mailboxes in each, and leaves out mail that is no longer pending.
func TestHeldBytesByMailbox(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	one, two := proofRoute("one@example.test", "one", 1), proofRoute("two@example.test", "two", 1)
	steps := []error{
		s.SetRoute(ctx, one), s.SetRoute(ctx, two),
		s.Bind(ctx, "g", "a", "", one.Address), s.Accept(ctx, "g", "a", "", strings.NewReader("12345")),
		s.Bind(ctx, "g", "b", "", one.Address), s.Bind(ctx, "g", "b", "", two.Address), s.Accept(ctx, "g", "b", "", strings.NewReader("123")),
		s.Bind(ctx, "g", "c", "", two.Address), s.Accept(ctx, "g", "c", "", strings.NewReader("1234567")),
	}
	for _, err := range steps {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE deliveries SET state='quarantined' WHERE id='c'"); err != nil {
		t.Fatal(err)
	}
	held, err := s.HeldBytesByMailbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if held["one"] != 8 || held["two"] != 3 || len(held) != 2 {
		t.Fatalf("held %v, want one 8, two 3", held)
	}
	for _, m := range []string{"one", "two"} {
		if n, err := s.HeldBytes(ctx, m); err != nil || n != held[m] {
			t.Errorf("HeldBytes(%s) = %d %v, by-mailbox %d", m, n, err, held[m])
		}
	}
}
