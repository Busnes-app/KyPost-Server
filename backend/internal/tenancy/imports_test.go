package tenancy

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

// RX R8 leases: one holder at a time, renewal by the holder only, expiry, and release
// keeping another holder's lease.
func TestImportLease(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_800_000_000, 0)
	take := func(holder string, at time.Time) bool {
		t.Helper()
		ok, err := TakeImportLease(ctx, db, "mbx", holder, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take("a", now) {
		t.Fatal("free lease refused")
	}
	if take("b", now.Add(ImportLease-time.Second)) {
		t.Fatal("live lease taken by another holder")
	}
	if !take("a", now.Add(30*time.Second)) {
		t.Fatal("holder could not renew")
	}
	if take("b", now.Add(ImportLease)) {
		t.Fatal("renewed lease taken before it expired")
	}
	if err := ReleaseImportLease(ctx, db, "mbx", "b"); err != nil {
		t.Fatal(err)
	}
	if take("b", now.Add(31*time.Second)) {
		t.Fatal("another holder's release freed the lease")
	}
	// A holder killed without releasing: its lease expires and another takes it, after
	// which the dead holder cannot renew.
	if !take("b", now.Add(30*time.Second+ImportLease)) {
		t.Fatal("expired lease not taken")
	}
	if take("a", now.Add(31*time.Second+ImportLease)) {
		t.Fatal("dead holder renewed a lease it lost")
	}
	if err := ReleaseImportLease(ctx, db, "mbx", "b"); err != nil {
		t.Fatal(err)
	}
	if !take("c", now.Add(31*time.Second+ImportLease)) {
		t.Fatal("released lease not free")
	}
	if ok, err := TakeImportLease(ctx, db, "", "c", now); ok || err == nil {
		t.Fatal("empty mailbox leased")
	}
	// Concurrent takers of a free lease: exactly one wins.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ok, err := TakeImportLease(ctx, db, "race", string(rune('a'+i)), now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d holders took one lease", wins.Load())
	}
}

func TestRetiredBindingsAndOrgImport(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_800_000_000, 0)
	for range 2 {
		if err := RecordRetired(ctx, db, "mx-a", "d1", "mbx", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		gateway, id, mailbox string
		want                 bool
	}{{"mx-a", "d1", "mbx", true}, {"mx-b", "d1", "mbx", false}, {"mx-a", "d1", "other", false}, {"mx-a", "d2", "mbx", false}} {
		if got, err := Retired(ctx, db, c.gateway, c.id, c.mailbox); err != nil || got != c.want {
			t.Fatalf("%+v: %v %v", c, got, err)
		}
	}

	const org, issuer = "1a2b3c4d-0000-4000-8000-00000000000a", "https://identity.example.test"
	imp := func(subject string) Import {
		t.Helper()
		i, err := MailboxImport(ctx, db, issuer, subject)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	if imp("one") != ImportWait {
		t.Fatal("a subject with no org imported")
	}
	if err := BindSubject(ctx, db, issuer, "one", org); err != nil {
		t.Fatal(err)
	}
	if imp("one") != ImportWait {
		t.Fatal("an org missing from the cache imported")
	}
	rev := int64(0)
	set := func(status string, enabled bool) {
		rev++
		if err := ApplyOrg(ctx, db, suitedir.Org{ID: org, Status: status, Revision: rev, Enabled: enabled, Quotas: json.RawMessage(`{}`)}, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		status  string
		enabled bool
	}{{suitedir.OrgActive, true}, {suitedir.OrgActive, false}, {suitedir.OrgSuspended, true}, {suitedir.OrgDeletionScheduled, true}} {
		set(s.status, s.enabled)
		if imp("one") != ImportNow {
			t.Fatalf("%+v did not import", s)
		}
	}
	set(suitedir.OrgDeleted, true)
	if imp("one") != ImportNever {
		t.Fatal("a deleted org imported")
	}
	// deleted is terminal: a later revision saying active does not resume import.
	set(suitedir.OrgActive, true)
	if imp("one") != ImportNever {
		t.Fatal("a deleted org came back")
	}
	// A stale cache still imports on the last known status.
	if o, err := LookupOrg(ctx, db, org); err != nil || o.Fresh(now.Add(StaleAfter+time.Minute)) || o.Import() != ImportNever {
		t.Fatal(o, err)
	}
}
