package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	take := func(holder string) bool {
		t.Helper()
		ok, err := TakeImportLease(ctx, db, "mbx", holder)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take("a") {
		t.Fatal("free lease refused")
	}
	if take("b") {
		t.Fatal("live lease taken by another holder")
	}
	if _, err := db.Exec(`UPDATE import_leases SET lease_until=unixepoch()+10 WHERE mailbox='mbx'`); err != nil {
		t.Fatal(err)
	}
	if !take("a") {
		t.Fatal("holder could not renew")
	}
	var left int64
	if err := db.QueryRow(`SELECT lease_until-unixepoch() FROM import_leases WHERE mailbox='mbx'`).Scan(&left); err != nil || left < 58 || left > 60 {
		t.Fatal("renewal did not extend on database time", left, err)
	}
	if take("b") {
		t.Fatal("renewed lease taken before it expired")
	}
	if err := ReleaseImportLease(ctx, db, "mbx", "b"); err != nil {
		t.Fatal(err)
	}
	if take("b") {
		t.Fatal("another holder's release freed the lease")
	}
	if _, err := db.Exec(`UPDATE import_leases SET lease_until=unixepoch()-1 WHERE mailbox='mbx'`); err != nil {
		t.Fatal(err)
	}
	if !take("b") {
		t.Fatal("expired lease not taken")
	}
	if take("a") {
		t.Fatal("dead holder renewed a lease it lost")
	}
	if err := ReleaseImportLease(ctx, db, "mbx", "b"); err != nil {
		t.Fatal(err)
	}
	if !take("c") {
		t.Fatal("released lease not free")
	}
	if ok, err := TakeImportLease(ctx, db, "", "c"); ok || err == nil {
		t.Fatal("empty mailbox leased")
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ok, err := TakeImportLease(ctx, db, "race", string(rune('a'+i)))
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

func TestImportLeaseRefusesInvalidOrCancelledRequests(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ok, err := TakeImportLease(ctx, nil, "mbx", "holder"); err == nil || ok {
		t.Fatal("nil store leased", ok, err)
	}
	for _, ids := range [][2]string{{"", "holder"}, {"mbx", ""}} {
		if ok, err := TakeImportLease(ctx, db, ids[0], ids[1]); err == nil || ok {
			t.Fatal("invalid lease", ids, ok, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := TakeImportLease(cancelled, db, "cancelled", "holder"); !errors.Is(err, context.Canceled) || ok {
		t.Fatal("cancelled request leased", ok, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM import_leases`).Scan(&count); err != nil || count != 0 {
		t.Fatal("refused request changed leases", count, err)
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

// One replica's fake clock starts in 2000; the other uses the host clock. Even
// advancing the fake clock beyond the lease must not change a database lease.
func TestImportLeaseIgnoresReplicaClockSkew(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fast, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	realNow := time.Now()
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(root, true)
		if err != nil {
			t.Fatal(err)
		}
		defer slow.Close()
		if time.Until(realNow) <= 2*ImportLease {
			t.Fatal("test clocks are not skewed beyond the lease")
		}
		if ok, err := TakeImportLease(ctx, slow, "skewed", "slow"); err != nil || !ok {
			t.Fatal(ok, err)
		}
	})
	if ok, err := TakeImportLease(ctx, fast, "skewed", "fast"); err != nil || ok {
		t.Fatal("fast replica stole a live lease", ok, err)
	}
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(root, true)
		if err != nil {
			t.Fatal(err)
		}
		defer slow.Close()
		time.Sleep(2 * ImportLease)
		if ok, err := TakeImportLease(ctx, slow, "skewed", "fast"); err != nil || ok {
			t.Fatal("local clock advancement expired a database lease", ok, err)
		}
		if ok, err := TakeImportLease(ctx, slow, "skewed", "slow"); err != nil || !ok {
			t.Fatal("slow holder could not renew", ok, err)
		}
	})
	var left int64
	if err := fast.QueryRow(`SELECT lease_until - unixepoch() FROM import_leases WHERE mailbox='skewed'`).Scan(&left); err != nil || left < 58 || left > 60 {
		t.Fatal("lease expiry differs from database time", left, err)
	}
	if _, err := fast.Exec(`UPDATE import_leases SET lease_until=unixepoch()-1 WHERE mailbox='skewed'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := TakeImportLease(ctx, fast, "skewed", "fast"); err != nil || !ok {
		t.Fatal("database-expired lease was not taken", ok, err)
	}
	synctest.Test(t, func(t *testing.T) {
		slow, err := Open(root, true)
		if err != nil {
			t.Fatal(err)
		}
		defer slow.Close()
		if ok, err := TakeImportLease(ctx, slow, "skewed", "slow"); err != nil || ok {
			t.Fatal("former slow holder reclaimed a live lease", ok, err)
		}
	})
}
