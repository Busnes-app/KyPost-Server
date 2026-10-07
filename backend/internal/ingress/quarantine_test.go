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
	if _, err = s.Discard(ctx, "maddy", "q"); !errors.Is(err, ErrNotQuarantined) {
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
	if _, err = s.Discard(ctx, "maddy", "q"); !errors.Is(err, ErrNotQuarantined) {
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
	if _, err = s.Discard(ctx, "maddy", "pending"); !errors.Is(err, ErrNotQuarantined) {
		t.Fatal("pending delivery discarded", err)
	}
	quarantined(t, s, "q")
	for range 2 {
		if disposition, err := s.Discard(ctx, "maddy", "q"); err != nil || disposition != "discarded" {
			t.Fatal(disposition, err)
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

// A discard after an interrupted release says some owners may have the mail,
// and the owner the release reached keeps exactly one copy.
func TestDiscardAfterInterruptedReleaseIsPartial(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	quarantined(t, s, "q")
	alice := openMailbox(t, root, mailbox.Owner{Issuer: "https://identity.example.test", Subject: "alice", Mailbox: "alice"})
	if err = s.Release(ctx, "maddy", "q", func(owner mailbox.Owner) (*mailbox.Store, error) {
		if owner.Subject == "alice" {
			return alice, nil
		}
		return nil, errors.New("bob disabled for good")
	}); err == nil {
		t.Fatal("interrupted release acknowledged")
	}
	if _, err = s.db.Exec("UPDATE deliveries SET lease_until=0"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if disposition, err := s.Discard(ctx, "maddy", "q"); err != nil || disposition != "partially_released" {
			t.Fatal(disposition, err)
		}
	}
	if d, err := s.Get(ctx, "maddy", "q"); err != nil || d.Disposition != "partially_released" || len(d.Raw) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	if list, err := alice.List(ctx, "INBOX", 0, 10); err != nil || len(list) != 1 {
		t.Fatal("reached owner lost or duplicated its copy", len(list), err)
	}
}

// A store whose tombstones predate dispositions gains the column, reading
// existing tombstones as imports, and keeps archiving.
func TestArchiveGainsDispositionColumn(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "holding")
	s, err := Open(dir, proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TABLE archived; CREATE TABLE archived (gateway TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, digest TEXT NOT NULL, recipients TEXT NOT NULL, archived_at INTEGER NOT NULL, PRIMARY KEY(gateway,id)) WITHOUT ROWID; INSERT INTO archived VALUES('maddy','old','sender@outside.test','digest','alice@example.test',1)"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if s, err = OpenExisting(dir, proofLimits); err != nil {
			t.Fatal(err)
		}
		if d, err := s.Get(ctx, "maddy", "old"); err != nil || d.State != "archived" || d.Disposition != "imported" {
			t.Fatalf("%+v %v", d, err)
		}
		var columns string
		if err := s.db.QueryRow(`SELECT group_concat(name||' '||type,',') FROM pragma_table_info('archived')`).Scan(&columns); err != nil || columns != ArchivedColumns {
			t.Fatal(columns, err)
		}
		_ = s.Close()
	}
	if s, err = OpenExisting(dir, proofLimits); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	quarantined(t, s, "q")
	if disposition, err := s.Discard(ctx, "maddy", "q"); err != nil || disposition != "discarded" {
		t.Fatal(disposition, err)
	}
}

// A hosted gateway's frozen binding quarantines with its bytes, from nothing
// or from staged, replays exactly and never takes a pending delivery.
func TestQuarantineFrozenBinding(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "holding"), Limits{MessageBytes: 1 << 10, PayloadBytes: 1 << 10, Records: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := []byte("Subject: frozen\r\n\r\nbytes\r\n")
	frozen := Binding{Address: "alice@example.test", Issuer: "https://issuer.test", Subject: "alice", Mailbox: "alice", Generation: 1}
	unresolved := Binding{Address: "alice@example.test", Generation: 4}
	for _, b := range []Binding{{Address: "alice@example.test", Subject: "alice", Generation: 1}, {Address: "alice@example.test", Mailbox: "alice"}} {
		if err := s.Quarantine(ctx, "hosted", "bad", "", b, raw); !errors.Is(err, ErrConflict) {
			t.Fatal("partial owner accepted", b, err)
		}
	}
	if err := s.Quarantine(ctx, "hosted", "new", "", unresolved, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(ctx, "hosted", "new", "", unresolved, raw); err != nil {
		t.Fatal("exact replay", err)
	}
	if err := s.Quarantine(ctx, "hosted", "new", "", unresolved, []byte("other")); !errors.Is(err, ErrConflict) {
		t.Fatal("changed bytes", err)
	}
	if err := s.SetRoute(ctx, Route{Address: frozen.Address, Issuer: frozen.Issuer, Subject: frozen.Subject, Mailbox: frozen.Mailbox, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, "hosted", "staged", "s@outside.test", frozen.Address); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(ctx, "hosted", "staged", "s@outside.test", unresolved, raw); !errors.Is(err, ErrConflict) {
		t.Fatal("staged binding replaced", err)
	}
	if err := s.Quarantine(ctx, "hosted", "staged", "s@outside.test", frozen, raw); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"new", "staged"} {
		d, err := s.Get(ctx, "hosted", id)
		if err != nil || d.State != "quarantined" || !bytes.Equal(d.Raw, raw) || len(d.Bindings) != 1 {
			t.Fatal(id, d.State, err)
		}
	}
	// Two records held: a third delivery is refused, never evicted.
	if err := s.Quarantine(ctx, "hosted", "third", "", unresolved, raw); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity", err)
	}
	if _, err := s.Discard(ctx, "hosted", "new"); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(ctx, "hosted", "new", "", unresolved, raw); err != nil {
		t.Fatal("discarded tombstone replay", err)
	}
	if err := s.Bind(ctx, "hosted", "pending", "", frozen.Address); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ctx, "hosted", "pending", "", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := s.Quarantine(ctx, "hosted", "pending", "", frozen, raw); !errors.Is(err, ErrConflict) {
		t.Fatal("pending delivery taken without QuarantinePending", err)
	}
}
