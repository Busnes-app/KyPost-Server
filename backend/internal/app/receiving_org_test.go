package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

const (
	receivingOrgA = "1a2b3c4d-0000-4000-8000-00000000000a"
	receivingOrgB = "1a2b3c4d-0000-4000-8000-00000000000b"
)

// multiRuntime gives r the multi-mode stores openReceivingRuntime opens: tenancy.db and
// state.db.
func multiRuntime(t *testing.T, r *receivingRuntime, db *sql.DB) {
	t.Helper()
	st, err := state.New(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.tenancy, r.state = db, st
}

// The org storage quotas at RCPT (tenancy design "Abuse limits per org"): mailboxBytes
// on the mailbox, storageBytes summed over the org's mailboxes only, and a subject with no
// recorded org refused. Every refusal is the mailbox-full exit 9.
func TestReceivingOrgStorageQuotas(t *testing.T) {
	r, created := receivingFixtureIn(t, receivingOrgA, receivingOrgB)
	ctx := context.Background()
	bind := func(id string) error { return r.bind(ctx, id, "sender@outside.test", "one@example.test") }
	full := func(name string, err error) {
		t.Helper()
		if receivingExit(err) != 9 || !errors.Is(err, errMailboxFull) {
			t.Fatalf("%s: %v, want the mailbox-full exit 9", name, err)
		}
	}

	cache, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	multiRuntime(t, r, cache)
	// A subject with no recorded org: K's org check answers first, temporarily (exit 12).
	if err := bind("unbound"); receivingExit(err) != 12 {
		t.Fatalf("unbound subject: %v, want exit 12", err)
	}
	for i, org := range []string{receivingOrgA, receivingOrgB} {
		u := created[i]
		if err := tenancy.BindSubject(ctx, cache, u.NativeMailboxIssuer, u.SSOSub, org); err != nil {
			t.Fatal(err)
		}
		if err := tenancy.BindUser(ctx, cache, u.ID, org); err != nil {
			t.Fatal(err)
		}
	}
	rev := int64(0)
	quotas := func(q string) {
		rev++
		if err := tenancy.ApplyOrg(ctx, cache, suitedir.Org{ID: receivingOrgA, Status: "active", Revision: rev, Enabled: true, Quotas: json.RawMessage(q)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	quotas(`{}`)
	if err := bind("defaults"); err != nil {
		t.Fatalf("no org quota: %v", err)
	}
	// RCPT needs room for one largest message (5 MiB here).
	quotas(`{"mailboxBytes":4194304}`)
	full("mailboxBytes", bind("mailbox-bytes"))
	quotas(`{"storageBytes":7340032}`)
	if err := bind("storage-empty"); err != nil {
		t.Fatalf("storage within quota: %v", err)
	}

	// 3 MiB in org B's mailbox does not count against org A.
	fill := func(i int) {
		a, _, err := r.life.NativeAssignment(created[i].NativeMailboxIssuer, created[i].SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		store, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", created[i].ID, "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		raw := append([]byte("From: test@outside.test\r\nSubject: filler\r\n\r\n"), bytes.Repeat([]byte("x"), 3<<20)...)
		if _, err := store.Append(ctx, "Archive", bytes.NewReader(raw), false); err != nil {
			t.Fatal(err)
		}
	}
	fill(1)
	if err := bind("storage-other-org"); err != nil {
		t.Fatalf("another org's mailbox counted: %v", err)
	}
	// 3 MiB in org A's mailbox does: 3 + 5 > 7 MiB.
	fill(0)
	full("storageBytes", bind("storage-full"))
	// Raising the quota admits mail again.
	quotas(`{"storageBytes":104857600}`)
	if err := bind("storage-raised"); err != nil {
		t.Fatal(err)
	}
}

// One delivery to two mailboxes of an org stores two copies: acceptance counts both
// against storageBytes, refusing with exit 9 and storing nothing when only one fits.
func TestReceivingOrgStorageCountsEveryCopy(t *testing.T) {
	r, created := receivingFixtureIn(t, receivingOrgA, receivingOrgA)
	ctx := context.Background()
	cache, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	multiRuntime(t, r, cache)
	for _, u := range created {
		if err := tenancy.BindSubject(ctx, cache, u.NativeMailboxIssuer, u.SSOSub, receivingOrgA); err != nil {
			t.Fatal(err)
		}
		if err := tenancy.BindUser(ctx, cache, u.ID, receivingOrgA); err != nil {
			t.Fatal(err)
		}
	}
	rev := int64(0)
	quota := func(q string) {
		rev++
		if err := tenancy.ApplyOrg(ctx, cache, suitedir.Org{ID: receivingOrgA, Status: "active", Revision: rev, Enabled: true, Quotas: json.RawMessage(q)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// RCPT needs room for one 5 MiB message per mailbox; the 3 MiB message fits each
	// mailbox alone, but two copies (6 MiB) do not fit 5.5 MiB.
	quota(`{"storageBytes":5767168}`)
	raw := append([]byte("From: sender@outside.test\r\nTo: one@example.test, two@example.test\r\nSubject: two copies\r\n\r\n"), bytes.Repeat([]byte("x"), 3<<20)...)
	deliver := func(id string) error {
		for _, address := range []string{"one@example.test", "two@example.test"} {
			if err := r.bind(ctx, id, "sender@outside.test", address); err != nil {
				t.Fatal(err)
			}
		}
		return r.accept(ctx, id, "sender@outside.test", bytes.NewReader(raw))
	}
	err = deliver("two-copies")
	if receivingExit(err) != 9 || !errors.Is(err, errMailboxFull) {
		t.Fatalf("two copies past storageBytes: %v, want exit 9", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "two-copies"); err == nil && len(d.Raw) != 0 {
		t.Fatalf("a refused delivery was stored: state %s", d.State)
	}
	quota(`{"storageBytes":7340032}`)
	if err := deliver("two-copies-fit"); err != nil {
		t.Fatalf("two copies within storageBytes: %v", err)
	}
}
