//go:build linux

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// RX PR 4: per-mailbox import obligations and leases (R8). Multi mode is refused at
// startup until K's PR 7, so these build the runtime with tenancy.db set directly, as
// K's own tests do. Accounts one and two are in orgs A and B.

var orgRevision atomic.Int64

// importFixture is receivingFixtureIn with tenancy.db, both subjects bound, and the
// given orgs cached active.
func importFixture(t *testing.T, cached ...string) (*receivingRuntime, []users.User) {
	t.Helper()
	r, created := receivingFixtureIn(t, receivingOrgA, receivingOrgB)
	db, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bindOrgs(t, db, created)
	multiRuntime(t, r, db)
	for _, org := range cached {
		setOrg(t, r.tenancy, org, suitedir.OrgActive)
	}
	return r, created
}

func bindOrgs(t *testing.T, db *sql.DB, created []users.User) {
	t.Helper()
	ctx := context.Background()
	for i, org := range []string{receivingOrgA, receivingOrgB} {
		if err := tenancy.BindSubject(ctx, db, created[i].NativeMailboxIssuer, created[i].SSOSub, org); err != nil {
			t.Fatal(err)
		}
		if err := tenancy.BindUser(ctx, db, created[i].ID, org); err != nil {
			t.Fatal(err)
		}
	}
}

func setOrg(t *testing.T, db *sql.DB, org, status string) {
	t.Helper()
	o := suitedir.Org{ID: org, Status: status, Revision: orgRevision.Add(1), Enabled: true, Quotas: json.RawMessage(`{}`)}
	if err := tenancy.ApplyOrg(context.Background(), db, o, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// deliver binds id to every address and accepts it; the subject is the ID.
func deliver(t *testing.T, r *receivingRuntime, id string, to ...string) {
	t.Helper()
	ctx := context.Background()
	for _, address := range to {
		if err := r.bind(ctx, id, tlsSender, address); err != nil {
			t.Fatalf("bind %s: %v", id, err)
		}
	}
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\nbody\r\n", tlsSender, to[0], id)
	if err := r.accept(ctx, id, tlsSender, bytes.NewReader([]byte(raw))); err != nil {
		t.Fatalf("accept %s: %v", id, err)
	}
}

func assignment(t *testing.T, r *receivingRuntime, u users.User) sso.NativeAssignment {
	t.Helper()
	a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// inbox is u's INBOX subjects, oldest first.
func inbox(t *testing.T, r *receivingRuntime, u users.User) []string {
	t.Helper()
	a := assignment(t, r, u)
	s, err := mailbox.OpenExisting(filepath.Join(a.Dir(r.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.List(context.Background(), "INBOX", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for i := len(list) - 1; i >= 0; i-- {
		subjects = append(subjects, list[i].Subject)
	}
	return subjects
}

func stateOf(t *testing.T, r *receivingRuntime, gateway, id string) string {
	t.Helper()
	d, err := r.holding.Get(context.Background(), gateway, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "gone"
	}
	if err != nil {
		t.Fatal(err)
	}
	return d.State
}

func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.NewWithOutput(&bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// pass is one import pass by holder.
func pass(t *testing.T, r *receivingRuntime, holder string) {
	t.Helper()
	r.importPass(context.Background(), holder, nil, testLogger(t))
}

// lease takes mailbox m's import lease for holder, as a worker does before writing.
func lease(t *testing.T, r *receivingRuntime, m, holder string) {
	t.Helper()
	if ok, err := tenancy.TakeImportLease(context.Background(), r.tenancy, m, holder); err != nil || !ok {
		t.Fatalf("lease %s for %s: %v %v", m, holder, ok, err)
	}
}

// Expire the database row without sleeping or supplying a replica's clock.
func expireImportLease(t *testing.T, r *receivingRuntime, m string) {
	t.Helper()
	if _, err := r.tenancy.Exec(`UPDATE import_leases SET lease_until=unixepoch()-1 WHERE mailbox=?`, m); err != nil {
		t.Fatal(err)
	}
}

func obligation(t *testing.T, r *receivingRuntime, m, id string) ingress.Obligation {
	t.Helper()
	page, err := r.holding.Obligations(context.Background(), m, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range page {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no obligation %s for %s", id, m)
	return ingress.Obligation{}
}

// purge runs K PR 6's purge steps 2 and 3 (api.PurgeReceiving, api.PurgeStorage) for
// members, whose org the test has marked deleted, then the daemon's Completion sweep,
// which archives a delivery the purge left with only finished bindings.
func purge(t *testing.T, r *receivingRuntime, members ...users.User) {
	t.Helper()
	ctx := context.Background()
	var ids, mailboxes []string
	for _, u := range members {
		ids = append(ids, u.ID)
		mailboxes = append(mailboxes, assignment(t, r, u).Owner.Mailbox)
	}
	if err := api.PurgeReceiving(ctx, r.holding, r.tenancy, mailboxes); err != nil {
		t.Fatal(err)
	}
	if err := api.PurgeStorage(ctx, r.configDir, r.stateDir, ids, mailboxes); err != nil {
		t.Fatal(err)
	}
	r.sweepCompletions(ctx, testLogger(t))
}

func removeStorage(dir string) error {
	if dbbackend.Postgres() {
		if err := dbbackend.Remove(dir); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// purgedClean is the purge's final check for receiving: no mailbox directory, no mailbox
// schema, so no receipt.
func purgedClean(t *testing.T, r *receivingRuntime, u users.User) {
	t.Helper()
	dir := assignment(t, r, u).Dir(r.stateDir)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mailbox directory left: %v", err)
	}
	if dbbackend.Postgres() {
		if held, err := dbbackend.Exists(filepath.Join(dir, "mailbox", "mailbox.db")); err != nil || held {
			t.Fatalf("mailbox schema left: %v %v", held, err)
		}
	}
}

// Arrival order per mailbox, nothing twice, with two replicas' workers racing.
func TestLeasedImportTwoReplicasKeepOrder(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	var want []string
	for i := range 20 {
		id := fmt.Sprintf("m%02d", i)
		deliver(t, r, id, "one@example.test")
		want = append(want, id)
	}
	other := *r // a second replica: its own runtime over the same stores
	for range 5 {
		var wg sync.WaitGroup
		wg.Go(func() { pass(t, r, "replica-a") })
		wg.Go(func() { pass(t, &other, "replica-b") })
		wg.Wait()
	}
	if got := inbox(t, r, created[0]); !slices.Equal(got, want) {
		t.Fatalf("order or duplicates: %v", got)
	}
	for _, id := range want {
		if s := stateOf(t, r, receivingGateway, id); s != "archived" {
			t.Fatalf("%s is %s", id, s)
		}
	}
}

// One delivery to orgs A and B, B deleted before import: A imports at once and keeps
// importing; B's obligation waits only for its purge, then the delivery is archived.
func TestLeasedImportDeletedOrgWaitsForItsPurge(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	deliver(t, r, "shared", "one@example.test", "two@example.test")
	setOrg(t, r.tenancy, receivingOrgB, suitedir.OrgDeleted)
	pass(t, r, "w")
	if got := inbox(t, r, created[0]); !slices.Equal(got, []string{"shared"}) {
		t.Fatalf("active org's mailbox: %v", got)
	}
	if s := stateOf(t, r, receivingGateway, "shared"); s != "pending" {
		t.Fatalf("shared is %s before the purge", s)
	}
	// A later revision cannot bring a deleted org back.
	setOrg(t, r.tenancy, receivingOrgB, suitedir.OrgActive)
	deliver(t, r, "later", "one@example.test")
	pass(t, r, "w")
	if got := inbox(t, r, created[0]); !slices.Equal(got, []string{"shared", "later"}) {
		t.Fatalf("active org's later mail: %v", got)
	}
	if got := inbox(t, r, created[1]); len(got) != 0 {
		t.Fatalf("deleted org imported: %v", got)
	}
	purge(t, r, created[1])
	if s := stateOf(t, r, receivingGateway, "shared"); s != "archived" {
		t.Fatalf("shared is %s after the purge", s)
	}
	d, _ := r.holding.Get(context.Background(), receivingGateway, "shared")
	if d.Disposition != "imported" {
		t.Fatalf("disposition %q", d.Disposition)
	}
	purgedClean(t, r, created[1])
}

// A suspended or deletion_scheduled org's accepted mail imports and is archived, so the
// shared ingress limits are freed.
func TestLeasedImportSuspendedOrgImports(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	deliver(t, r, "to-suspended", "one@example.test")
	deliver(t, r, "to-scheduled", "two@example.test")
	setOrg(t, r.tenancy, receivingOrgA, suitedir.OrgSuspended)
	setOrg(t, r.tenancy, receivingOrgB, suitedir.OrgDeletionScheduled)
	pass(t, r, "w")
	for i, id := range []string{"to-suspended", "to-scheduled"} {
		if s := stateOf(t, r, receivingGateway, id); s != "archived" {
			t.Fatalf("%s is %s", id, s)
		}
		if got := inbox(t, r, created[i]); !slices.Equal(got, []string{id}) {
			t.Fatalf("%s: %v", id, got)
		}
	}
	if held, err := r.holding.HeldBytes(context.Background(), assignment(t, r, created[0]).Owner.Mailbox); err != nil || held != 0 {
		t.Fatalf("held bytes %d %v", held, err)
	}
}

// An org missing from the cache: its mailbox waits, the other mailbox imports, and the
// first good poll releases it. Bind refuses an uncached org (exit 12), so the mail was
// accepted before a restore emptied the cache (tenancy.ForgetRestored).
func TestLeasedImportUncachedOrgWaits(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	deliver(t, r, "to-a", "one@example.test")
	deliver(t, r, "to-b", "two@example.test")
	if err := tenancy.ForgetRestored(context.Background(), r.tenancy); err != nil {
		t.Fatal(err)
	}
	setOrg(t, r.tenancy, receivingOrgA, suitedir.OrgActive)
	pass(t, r, "w")
	if s := stateOf(t, r, receivingGateway, "to-a"); s != "archived" {
		t.Fatalf("to-a is %s", s)
	}
	if s := stateOf(t, r, receivingGateway, "to-b"); s != "pending" {
		t.Fatalf("to-b is %s before its org is cached", s)
	}
	setOrg(t, r.tenancy, receivingOrgB, suitedir.OrgActive)
	pass(t, r, "w")
	if got := inbox(t, r, created[1]); !slices.Equal(got, []string{"to-b"}) {
		t.Fatalf("to-b after the poll: %v", got)
	}
}

// An org deleted between a worker's first status check and its write. The write re-reads
// the org under the quota lock and writes nothing; or it finished first and the purge
// removes it. Either way the purge leaves no mailbox, receipt or schema.
func TestLeasedImportPurgeFence(t *testing.T) {
	ctx := context.Background()
	t.Run("write sees deleted", func(t *testing.T) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		deliver(t, r, "late", "one@example.test")
		a := assignment(t, r, created[0])
		d, err := r.holding.Get(ctx, receivingGateway, "late")
		if err != nil {
			t.Fatal(err)
		}
		setOrg(t, r.tenancy, receivingOrgA, suitedir.OrgDeleted)
		// writeMailbox is the step after the first check: only the re-read can stop it.
		lease(t, r, a.Owner.Mailbox, "w")
		if err := r.writeMailbox(ctx, "w", d, a.Owner, d.Bindings); !errors.Is(err, tenancy.ErrImportDeleted) {
			t.Fatalf("write for a deleted org: %v", err)
		}
		if got := inbox(t, r, created[0]); len(got) != 0 {
			t.Fatalf("written: %v", got)
		}
		purge(t, r, created[0])
		if d, err := r.holding.Get(ctx, receivingGateway, "late"); err != nil || d.State != "archived" || d.Disposition != "discarded" {
			t.Fatalf("late: %+v %v", d, err)
		}
		purgedClean(t, r, created[0])
	})
	t.Run("purge holds the lock", func(t *testing.T) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		deliver(t, r, "raced", "one@example.test")
		a := assignment(t, r, created[0])
		o := obligation(t, r, a.Owner.Mailbox, "raced")
		release, err := fsutil.LockFileContext(ctx, filepath.Join(r.stateDir, "receiving", "quota-"+a.Owner.Mailbox))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		lease(t, r, a.Owner.Mailbox, "w")
		go func() { _, err := r.importObligation(ctx, "w", o, a.Owner.Mailbox); done <- err }()
		time.Sleep(300 * time.Millisecond) // the worker has passed its first check and waits for the lock
		setOrg(t, r.tenancy, receivingOrgA, suitedir.OrgDeleted)
		if err := removeStorage(a.Dir(r.stateDir)); err != nil {
			t.Fatal(err)
		}
		release()
		if err := <-done; !errors.Is(err, tenancy.ErrImportDeleted) && !errors.Is(err, errImportWait) {
			t.Fatalf("worker after the purge's lock: %v", err)
		}
		purge(t, r, created[0])
		purgedClean(t, r, created[0])
	})
	t.Run("lease lost while waiting", func(t *testing.T) {
		// A worker whose lease ran out while it waited for the quota lock writes nothing,
		// so it cannot overtake the mailbox's new holder.
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		deliver(t, r, "stalled", "one@example.test")
		a := assignment(t, r, created[0])
		m := a.Owner.Mailbox
		o := obligation(t, r, m, "stalled")
		release, err := fsutil.LockFileContext(ctx, filepath.Join(r.stateDir, "receiving", "quota-"+m))
		if err != nil {
			t.Fatal(err)
		}
		lease(t, r, m, "slow")
		done := make(chan error, 1)
		go func() { _, err := r.importObligation(ctx, "slow", o, m); done <- err }()
		time.Sleep(300 * time.Millisecond)
		expireImportLease(t, r, m)
		lease(t, r, m, "new")
		release()
		if err := <-done; !errors.Is(err, errLeaseLost) {
			t.Fatalf("stalled worker: %v", err)
		}
		if got := inbox(t, r, created[0]); len(got) != 0 {
			t.Fatalf("written after the lease was lost: %v", got)
		}
		if ok, err := tenancy.Retired(ctx, r.tenancy, receivingGateway, "stalled", m); err != nil || ok {
			t.Fatalf("lost lease recorded as retired: %v %v", ok, err)
		}
	})
	t.Run("write first", func(t *testing.T) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		deliver(t, r, "early", "one@example.test")
		pass(t, r, "w")
		if got := inbox(t, r, created[0]); !slices.Equal(got, []string{"early"}) {
			t.Fatalf("early: %v", got)
		}
		setOrg(t, r.tenancy, receivingOrgA, suitedir.OrgDeleted)
		purge(t, r, created[0])
		purgedClean(t, r, created[0])
	})
}

// A worker killed between its mailbox write and Completion: the retry adds no second
// copy, and the backstop sweep completes the delivery.
func TestLeasedImportCrashBeforeCompletion(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	ctx := context.Background()
	deliver(t, r, "crashed", "one@example.test", "two@example.test")
	d, err := r.holding.Get(ctx, receivingGateway, "crashed")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range created {
		m := assignment(t, r, u).Owner.Mailbox
		lease(t, r, m, "crashed")
		if err := r.writeObligation(ctx, "crashed", d, m); err != nil {
			t.Fatal(err)
		}
		// Its lease runs out.
		expireImportLease(t, r, m)
	}
	pass(t, r, "retry")
	for _, u := range created {
		if got := inbox(t, r, u); !slices.Equal(got, []string{"crashed"}) {
			t.Fatalf("retry copies: %v", got)
		}
	}
	if s := stateOf(t, r, receivingGateway, "crashed"); s != "pending" {
		t.Fatalf("crashed is %s before the sweep", s)
	}
	r.sweepCompletions(ctx, testLogger(t))
	if s := stateOf(t, r, receivingGateway, "crashed"); s != "archived" {
		t.Fatalf("crashed is %s after the sweep", s)
	}
}

// Two workers finishing the last two obligations of one delivery at the same moment:
// it is archived exactly once, and neither fails.
func TestLeasedImportConcurrentCompletion(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	ctx := context.Background()
	for i := range 5 {
		id := fmt.Sprintf("both-%d", i)
		deliver(t, r, id, "one@example.test", "two@example.test")
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, u := range created {
			m := assignment(t, r, u).Owner.Mailbox
			o := obligation(t, r, m, id)
			lease(t, r, m, "w")
			wg.Go(func() {
				<-start
				if _, err := r.importObligation(ctx, "w", o, m); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		wg.Wait()
		for _, u := range created {
			if err := tenancy.ReleaseImportLease(ctx, r.tenancy, assignment(t, r, u).Owner.Mailbox, "w"); err != nil {
				t.Fatal(err)
			}
		}
		d, err := r.holding.Get(ctx, receivingGateway, id)
		if err != nil || d.State != "archived" || d.Disposition != "imported" {
			t.Fatalf("%s: %+v %v", id, d, err)
		}
	}
	for _, u := range created {
		if got := inbox(t, r, u); len(got) != 5 {
			t.Fatalf("copies: %v", got)
		}
	}
}

// One delivery to an active and a retired binding: the active one imports, the retired
// one is recorded and the delivery is quarantined with only that binding, listed in staff
// quarantine. A purge that leaves only retired rows quarantines too.
func TestLeasedImportRetiredBinding(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*receivingRuntime, []users.User) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, "sales@example.test"); err != nil {
			t.Fatal(err)
		}
		deliver(t, r, "mixed", "sales@example.test", "two@example.test")
		// sales@ is released after acceptance: its binding to mailbox one can never import.
		if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "sales@example.test"); err != nil {
			t.Fatal(err)
		}
		return r, created
	}
	quarantinedWith := func(t *testing.T, r *receivingRuntime, address string) {
		t.Helper()
		q, err := r.holding.ListQuarantined(ctx, 0, 10)
		if err != nil || len(q) != 1 || q[0].ID != "mixed" || len(q[0].Bindings) != 1 || q[0].Bindings[0].Address != address {
			t.Fatalf("staff quarantine: %+v %v", q, err)
		}
	}
	t.Run("active and retired", func(t *testing.T) {
		r, created := setup(t)
		pass(t, r, "w")
		if got := inbox(t, r, created[1]); !slices.Equal(got, []string{"mixed"}) {
			t.Fatalf("active binding: %v", got)
		}
		if got := inbox(t, r, created[0]); len(got) != 0 {
			t.Fatalf("retired binding imported: %v", got)
		}
		if ok, err := tenancy.Retired(ctx, r.tenancy, receivingGateway, "mixed", assignment(t, r, created[0]).Owner.Mailbox); err != nil || !ok {
			t.Fatalf("retired row: %v %v", ok, err)
		}
		if s := stateOf(t, r, receivingGateway, "mixed"); s != "quarantined" {
			t.Fatalf("mixed is %s", s)
		}
		quarantinedWith(t, r, "sales@example.test")
	})
	t.Run("purge leaves only retired", func(t *testing.T) {
		r, created := setup(t)
		setOrg(t, r.tenancy, receivingOrgB, suitedir.OrgDeleted)
		pass(t, r, "w")
		if s := stateOf(t, r, receivingGateway, "mixed"); s != "pending" {
			t.Fatalf("mixed is %s before the purge", s)
		}
		purge(t, r, created[1])
		if s := stateOf(t, r, receivingGateway, "mixed"); s != "quarantined" {
			t.Fatalf("mixed is %s after the purge", s)
		}
		quarantinedWith(t, r, "sales@example.test")
	})
}

// A lease holder killed mid-batch blocks its mailbox only until the lease expires; and a
// mailbox under constant load gives its lease up after each batch of 100.
func TestLeasedImportLeases(t *testing.T) {
	ctx := context.Background()
	t.Run("holder killed mid-batch", func(t *testing.T) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		m := assignment(t, r, created[0]).Owner.Mailbox
		for _, id := range []string{"k1", "k2", "k3"} {
			deliver(t, r, id, "one@example.test")
		}
		if ok, err := tenancy.TakeImportLease(ctx, r.tenancy, m, "dead"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		d, err := r.holding.Get(ctx, receivingGateway, "k1")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.writeObligation(ctx, "dead", d, m); err != nil { // killed before Completion
			t.Fatal(err)
		}
		pass(t, r, "live")
		if got := inbox(t, r, created[0]); !slices.Equal(got, []string{"k1"}) {
			t.Fatalf("imported under another holder's lease: %v", got)
		}
		// The dead holder's lease runs out.
		expireImportLease(t, r, m)
		pass(t, r, "live")
		r.sweepCompletions(ctx, testLogger(t))
		if got := inbox(t, r, created[0]); !slices.Equal(got, []string{"k1", "k2", "k3"}) {
			t.Fatalf("after expiry: %v", got)
		}
		for _, id := range []string{"k1", "k2", "k3"} {
			if s := stateOf(t, r, receivingGateway, id); s != "archived" {
				t.Fatalf("%s is %s", id, s)
			}
		}
	})
	t.Run("constant load", func(t *testing.T) {
		r, created := importFixture(t, receivingOrgA, receivingOrgB)
		m := assignment(t, r, created[0]).Owner.Mailbox
		for i := range importBatch + 1 {
			deliver(t, r, fmt.Sprintf("load-%03d", i), "one@example.test")
		}
		logger := testLogger(t)
		next := r.importPass(ctx, "w", nil, logger)
		if !next[m] || len(inbox(t, r, created[0])) != importBatch {
			t.Fatalf("batch bound: next=%v imported=%d", next, len(inbox(t, r, created[0])))
		}
		var leases int
		if err := r.tenancy.QueryRow("SELECT COUNT(*) FROM import_leases").Scan(&leases); err != nil || leases != 0 {
			t.Fatalf("lease kept after the batch: %d %v", leases, err)
		}
		// Skipped for one pass, so another worker can take it; then the rest imports.
		if next = r.importPass(ctx, "w", next, logger); len(next) != 0 || len(inbox(t, r, created[0])) != importBatch {
			t.Fatal("not skipped for one pass")
		}
		r.importPass(ctx, "w", next, logger)
		if got := inbox(t, r, created[0]); len(got) != importBatch+1 || got[importBatch] != fmt.Sprintf("load-%03d", importBatch) {
			t.Fatalf("rest: %d", len(got))
		}
	})
}

// Single mode starts no leased import, never creates tenancy.db, and keeps the daemon
// importer's all-or-nothing import.
func TestSingleModeKeepsTheDaemonImporter(t *testing.T) {
	r, created := receivingFixture(t)
	quarantineCLI(t, r)
	t.Setenv("KYPOST_TENANCY", "")
	t.Setenv("KYPOST_STAFF_ORG_ID", "")
	done, err := startLeasedImport(context.Background(), runDeps{nativeReceiving: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("single mode started leased import")
	}
	if _, err := os.Lstat(filepath.Join(r.stateDir, tenancy.File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tenancy.db in single mode: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	deps := runDeps{nativeReceiving: true, logger: testLogger(t)}
	imported, err := startReceivingImport(ctx, deps)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, r, "single", "one@example.test", "two@example.test")
	deadline := time.Now().Add(20 * time.Second)
	for stateOf(t, r, receivingGateway, "single") != "archived" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-imported
	for _, u := range created {
		if got := inbox(t, r, u); !slices.Equal(got, []string{"single"}) {
			t.Fatalf("daemon importer: %v", got)
		}
	}
	if _, err := os.Lstat(filepath.Join(r.stateDir, tenancy.File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tenancy.db in single mode: %v", err)
	}
}

// Mail a gateway committed still imports after its pin is revoked or expires: the import
// list is every gateway with pending mail, never the live pins. The ID's other pin keeps
// working, a second rotation under the ID is accepted, a host whose pins all expired is
// re-enrolled under its own ID with a new key, and a revoked key or a retired ID is refused.
func TestLeasedImportOutlivesItsGatewayPin(t *testing.T) {
	f := multiReceivingFixture(t)
	ctx := context.Background()
	const issuer = "https://identity.example.test"
	for subject, org := range map[string]string{"one": receivingOrgA, "two": receivingOrgB} {
		if err := tenancy.BindSubject(ctx, f.db, issuer, subject, org); err != nil {
			t.Fatal(err)
		}
		setOrg(t, f.db, org, suitedir.OrgActive)
	}
	multiRuntime(t, f.r, f.db)
	addr, _ := f.serve(t)
	send := func(key, id string) error {
		t.Helper()
		f.remote(t, key, []string{addr})
		if err := runReceivingCommand([]string{"bind", id, tlsSender, "one@example.test"}, nil); err != nil {
			return err
		}
		return runReceivingCommand([]string{"accept", id, tlsSender}, bytes.NewReader(tlsMessage))
	}
	gateway := func(args ...string) error { return runReceivingGateway(args, io.Discard) }

	keyA1, pinA1 := f.key(t, "a1", "mx-a")
	keyA2, _ := f.key(t, "a2", "mx-a")
	if err := send(keyA1, "tx-revoked"); err != nil {
		t.Fatal(err)
	}
	if err := gateway("revoke", "mx-a", pinA1); err != nil {
		t.Fatal(err)
	}
	if send(keyA1, "tx-refused") == nil {
		t.Fatal("revoked pin forwarded")
	}
	if err := send(keyA2, "tx-other-pin"); err != nil {
		t.Fatal("the ID's other pin:", err)
	}
	f.key(t, "a3", "mx-a") // a second rotation under mx-a: f.key fails the test if refused

	keyB1 := filepath.Join(f.keys, "b1.key")
	var out bytes.Buffer
	if err := runReceivingGateway([]string{"keygen", keyB1}, &out); err != nil {
		t.Fatal(err)
	}
	pinB1 := strings.TrimSpace(out.String())
	// Enrolled so that it expires two seconds from now.
	if _, err := tenancy.AddGateway(ctx, f.db, "mx-b", pinB1, time.Now().Add(2*time.Second-tenancy.PinLifetime)); err != nil {
		t.Fatal(err)
	}
	if err := send(keyB1, "tx-expired"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if send(keyB1, "tx-after-expiry") == nil {
		t.Fatal("expired pin forwarded")
	}

	pass(t, f.r, "w")
	for _, c := range []struct{ gateway, id string }{{"mx-a", "tx-revoked"}, {"mx-a", "tx-other-pin"}, {"mx-b", "tx-expired"}} {
		if s := stateOf(t, f.r, c.gateway, c.id); s != "archived" {
			t.Fatalf("%s/%s is %s", c.gateway, c.id, s)
		}
	}
	if got := inbox(t, f.r, users.User{NativeMailboxIssuer: issuer, SSOSub: "one"}); len(got) != 3 {
		t.Fatalf("imported %d of 3", len(got))
	}

	// The host whose only pin expired: a new key under its own ID; the old key stays refused.
	keyB2, _ := f.key(t, "b2", "mx-b")
	if err := send(keyB2, "tx-reenrolled"); err != nil {
		t.Fatal("re-enrolled host:", err)
	}
	if gateway("add", "mx-b", pinB1) == nil {
		t.Fatal("expired key re-enrolled")
	}
	if gateway("add", "mx-c", pinA1) == nil {
		t.Fatal("revoked key enrolled under another ID")
	}
	if err := gateway("retire", "mx-a"); err != nil {
		t.Fatal(err)
	}
	_, pinA4 := f.key(t, "a4", "")
	if gateway("add", "mx-a", pinA4) == nil {
		t.Fatal("retired ID enrolled")
	}
}
