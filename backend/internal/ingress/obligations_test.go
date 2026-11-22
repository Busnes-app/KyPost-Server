package ingress

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// Multi-mode obligations (receiving tier R8): listing by mailbox in arrival order across
// gateways, one mailbox's copy without a claim, and Completion's three outcomes.
func TestObligations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	createImport(t, s, "first")
	createImport(t, s, "second")
	alice := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "alice", Mailbox: "alice"}
	bob := mailbox.Owner{Issuer: "https://identity.example.test", Subject: "bob", Mailbox: "bob"}

	if gw, err := s.PendingGateways(ctx); err != nil || !slices.Equal(gw, []string{"maddy"}) {
		t.Fatalf("pending gateways %v %v", gw, err)
	}
	if m, err := s.PendingMailboxes(ctx); err != nil || !slices.Equal(m, []string{"alice", "bob"}) {
		t.Fatalf("pending mailboxes %v %v", m, err)
	}
	page, err := s.Obligations(ctx, "alice", 0, 100)
	if err != nil || len(page) != 2 || page[0].ID != "first" || page[1].ID != "second" {
		t.Fatalf("arrival order %+v %v", page, err)
	}
	if next, err := s.Obligations(ctx, "alice", page[0].Sequence, 100); err != nil || len(next) != 1 || next[0].ID != "second" {
		t.Fatalf("paging %+v %v", next, err)
	}
	if _, err := s.Obligations(ctx, "alice", 0, 101); !errors.Is(err, ErrConflict) {
		t.Fatalf("oversize page: %v", err)
	}

	d, err := s.Get(ctx, "maddy", "first")
	if err != nil {
		t.Fatal(err)
	}
	a := openMailbox(t, root, alice)
	for range 2 { // a repeat finds the receipt
		if err := s.ImportMailbox(ctx, d, "INBOX", a); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := a.List(ctx, "INBOX", 0, 10); err != nil || len(list) != 1 {
		t.Fatalf("one copy for both of alice's addresses: %+v %v", list, err)
	}
	// A store that is not a bound mailbox's is refused.
	if err := s.ImportMailbox(ctx, d, "INBOX", openMailbox(t, root, mailbox.Owner{Issuer: alice.Issuer, Subject: "carol", Mailbox: "carol"})); !errors.Is(err, ErrRoute) {
		t.Fatalf("unbound mailbox: %v", err)
	}
	forged := d
	forged.Bindings = slices.Clone(d.Bindings)
	for i := range forged.Bindings {
		if forged.Bindings[i].Mailbox == "alice" {
			forged.Bindings[i].Subject = "mallory"
		}
	}
	if err := s.ImportMailbox(ctx, forged, "INBOX", a); !errors.Is(err, mailbox.ErrOwner) {
		t.Fatalf("binding with another owner: %v", err)
	}
	tampered := d
	tampered.Raw = append([]byte("X"), d.Raw...)
	if err := s.ImportMailbox(ctx, tampered, "INBOX", a); !errors.Is(err, ErrConflict) {
		t.Fatalf("bytes that do not match the digest: %v", err)
	}

	// Completion: nothing while bob is open; archived when both imported.
	if state, err := s.Complete(ctx, "maddy", "first", map[string]Done{"alice": Imported}); err != nil || state != "pending" {
		t.Fatalf("open obligation completed: %s %v", state, err)
	}
	if err := s.ImportMailbox(ctx, d, "INBOX", openMailbox(t, root, bob)); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second caller finds it ended
		state, err := s.Complete(ctx, "maddy", "first", map[string]Done{"alice": Imported, "bob": Imported})
		if err != nil || state != "archived" && state != "" {
			t.Fatalf("complete: %s %v", state, err)
		}
	}
	if got, err := s.Get(ctx, "maddy", "first"); err != nil || got.State != "archived" || got.Disposition != "imported" {
		t.Fatalf("not archived as imported: %+v %v", got, err)
	}

	// A retired binding quarantines the delivery with only that binding.
	if state, err := s.Complete(ctx, "maddy", "second", map[string]Done{"alice": Imported, "bob": Retired}); err != nil || state != "quarantined" {
		t.Fatalf("retired: %s %v", state, err)
	}
	q, err := s.ListQuarantined(ctx, 0, 10)
	if err != nil || len(q) != 1 || len(q[0].Bindings) != 1 || q[0].Bindings[0].Mailbox != "bob" {
		t.Fatalf("quarantine keeps only the retired binding: %+v %v", q, err)
	}
	if m, err := s.PendingMailboxes(ctx); err != nil || len(m) != 0 {
		t.Fatalf("nothing pending: %v %v", m, err)
	}
	if err := s.ImportMailbox(ctx, Delivery{State: "quarantined"}, "INBOX", a); !errors.Is(err, ErrLease) {
		t.Fatalf("a quarantined delivery imported: %v", err)
	}
}

// Completion leaves a delivery under single mode's live whole-delivery claim alone.
func TestCompleteLeavesAClaimedDelivery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	createImport(t, s, "claimed")
	if _, err := s.Claim(ctx, "maddy", "claimed", time.Minute); err != nil {
		t.Fatal(err)
	}
	if state, err := s.Complete(ctx, "maddy", "claimed", map[string]Done{"alice": Imported, "bob": Imported}); err != nil || state != "pending" {
		t.Fatalf("claimed delivery completed: %s %v", state, err)
	}
}
