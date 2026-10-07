package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// quarantined builds a delivery frozen to alice (two aliases) and bob, then
// moves alice's address to mallory so the import quarantines it.
func quarantined(t *testing.T, s *Store, id string) {
	t.Helper()
	createImport(t, s, id)
	if err := s.SetRoute(context.Background(), proofRoute("alice@example.test", "mallory", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(context.Background(), "maddy", id, time.Minute); !errors.Is(err, ErrRoute) {
		t.Fatal("not quarantined", err)
	}
}

// Release reaches the frozen owners despite the newer route, survives an
// interrupted first attempt without duplicates, and repeats harmlessly.
func TestReleaseDeliversToFrozenOwnersOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	quarantined(t, s, "q")
	stores := map[string]*mailbox.Store{}
	resolve := func(fail string) func(mailbox.Owner) (*mailbox.Store, error) {
		return func(owner mailbox.Owner) (*mailbox.Store, error) {
			if owner.Subject == fail {
				return nil, errors.New("crash")
			}
			if stores[owner.Subject] == nil {
				stores[owner.Subject] = openMailbox(t, root, owner)
			}
			return stores[owner.Subject], nil
		}
	}
	if err = s.Import(ctx, "maddy", "q", resolve("")); !errors.Is(err, ErrLease) {
		t.Fatal("importer took a quarantined delivery", err)
	}
	if err = s.Release(ctx, "maddy", "q", resolve("bob")); err == nil {
		t.Fatal("interrupted release acknowledged")
	}
	if err = s.Discard(ctx, "maddy", "q"); !errors.Is(err, ErrNotQuarantined) {
		t.Fatal("discard raced a live release lease", err)
	}
	if _, err = s.db.Exec("UPDATE deliveries SET lease_until=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.Release(ctx, "maddy", "q", resolve("")); err != nil {
		t.Fatal(err)
	}
	if err = s.Release(ctx, "maddy", "q", resolve("")); !errors.Is(err, ErrLease) {
		t.Fatal("archived delivery claimed again", err)
	}
	d, err := s.Get(ctx, "maddy", "q")
	if err != nil || d.State != "archived" || d.Disposition != "released" || len(d.Raw) != 0 {
		t.Fatalf("release not archived: %+v %v", d, err)
	}
	if stores["mallory"] != nil {
		t.Fatal("release resolved the address's current owner")
	}
	for owner, store := range stores {
		list, err := store.List(ctx, "INBOX", 0, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: want one copy, got %d %v", owner, len(list), err)
		}
	}
	if err = s.Discard(ctx, "maddy", "q"); !errors.Is(err, ErrNotQuarantined) {
		t.Fatal("released delivery discarded", err)
	}
}

// Discard keeps a tombstone that answers replays without reopening.
func TestDiscardTombstoneBlocksRepickup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createImport(t, s, "pending")
	if err = s.Discard(ctx, "maddy", "pending"); !errors.Is(err, ErrNotQuarantined) {
		t.Fatal("pending delivery discarded", err)
	}
	quarantined(t, s, "q")
	for range 2 {
		if err = s.Discard(ctx, "maddy", "q"); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.Get(ctx, "maddy", "q")
	if err != nil || d.State != "archived" || d.Disposition != "discarded" || len(d.Raw) != 0 || d.Bindings != nil {
		t.Fatalf("discard left bytes or no tombstone: %+v %v", d, err)
	}
	// A re-pickup replays exact bind/accept and still cannot deliver.
	if err = s.Bind(ctx, "maddy", "q", "sender@outside.test", "hidden@example.test"); err != nil {
		t.Fatal(err)
	}
	if err = s.Accept(ctx, "maddy", "q", "sender@outside.test", bytes.NewReader(importRaw)); err != nil {
		t.Fatal(err)
	}
	if err = s.Bind(ctx, "maddy", "q", "sender@outside.test", "new@example.test"); !errors.Is(err, ErrConflict) {
		t.Fatal("tombstone gained a recipient", err)
	}
	if err = s.Release(ctx, "maddy", "q", func(mailbox.Owner) (*mailbox.Store, error) { return nil, errors.New("resolved") }); !errors.Is(err, ErrLease) {
		t.Fatal("discarded delivery released", err)
	}
	if d, err = s.Get(ctx, "maddy", "q"); err != nil || d.Disposition != "discarded" {
		t.Fatal("replay changed disposition", d.Disposition, err)
	}
}

// Listing carries envelopes and never the message bytes.
func TestListQuarantinedIsEnvelopeOnly(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createImport(t, s, "pending")
	quarantined(t, s, "q")
	rows, err := s.ListQuarantined(ctx, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].ID != "q" || rows[0].Size != int64(len(importRaw)) || rows[0].Sender != "sender@outside.test" || len(rows[0].Bindings) != 3 || rows[0].Received.IsZero() {
		t.Fatalf("%+v %v", rows, err)
	}
	if text := fmt.Sprintf("%+v", rows); bytes.Contains([]byte(text), []byte("shared receipt")) || bytes.Contains([]byte(text), []byte("raw body")) {
		t.Fatal("listing leaked message content", text)
	}
	if rows, err = s.ListQuarantined(ctx, rows[0].Sequence, 100); err != nil || len(rows) != 0 {
		t.Fatal("paging", rows, err)
	}
}
