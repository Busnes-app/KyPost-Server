package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var errKilled = errors.New("killed by the test")

const purgeGateway = "qualified-test"

// purgeFixture is the cross-org fixture with org A's members holding a session, a contact,
// a device, an agent token, a scheduled send, a send charge, a message, a pickup record, a
// delivery of their own, one shared with org B and one already imported.
type purgeFixture struct {
	*orgFixture
	db       *sql.DB
	bDigest  string
	aAddress []string
}

func newPurgeFixture(t *testing.T) *purgeFixture {
	t.Helper()
	fx, _ := newOrgFixture(t)
	withAuditLog(t, fx.srv)
	px := &purgeFixture{orgFixture: fx, db: fx.srv.tenancy}
	ctx := context.Background()
	for _, m := range []orgMember{fx.a, fx.other} {
		mintSessionForTest(fx.srv, m.ID)
		seedContact(fx, m)
		seedDevice(fx, m)
		seedOrgAgentToken(fx, m)
		fx.message(m)
		check(fx, fx.srv.globalStore.AddScheduledSend(ctx, state.ScheduledSend{ID: newID(fx), UserID: m.ID, Mailbox: m.ID, Status: state.ScheduledWaiting, SendAt: time.Now().Add(time.Hour).Unix(), CreatedAt: time.Now().Unix(), Sealed: []byte("x"), Summary: []byte("{}")}, 10, 1<<20))
		_, err := fx.srv.globalStore.AdmitSend(ctx, newID(fx), m.ID, 1, 60, 1000, time.Now().Unix())
		check(fx, err)
		_, err = fx.srv.pickupStore.Create(m.ID, "reader@outside.test", "subject", "body", "plain", time.Hour)
		check(fx, err)
	}
	px.aAddress = []string{"member-a@example.test", "member-a-box@example.test", "member-a2@example.test", "member-a2-box@example.test"}
	holding, err := ingress.Open(filepath.Join(fx.srv.stateDir, "receiving"), ingress.ReceivingLimits)
	check(fx, err)
	defer holding.Close()
	route := func(address, subject, mailbox string) {
		check(fx, holding.SetRoute(ctx, ingress.Route{Address: address, Issuer: fx.srv.ssoStore.Load().IssuerURL, Subject: subject, Mailbox: mailbox, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Hour)}))
	}
	route("member-a@example.test", "member-a", fx.a.ID)
	route("member-a-box@example.test", "member-a", fx.a.extra)
	route("member-b@example.test", "member-b", fx.b.ID)
	deliver := func(id string, to ...string) {
		for _, rcpt := range to {
			check(fx, holding.Bind(ctx, purgeGateway, id, "sender@outside.test", rcpt))
		}
		raw := mailmsg.Message{From: "sender@outside.test", To: to, Subject: "seed", Body: "seed"}.Build()
		check(fx, holding.Accept(ctx, purgeGateway, id, "sender@outside.test", strings.NewReader(string(raw))))
	}
	deliver("a-only", "member-a@example.test", "member-a-box@example.test")
	deliver("shared", "member-a@example.test", "member-b@example.test")
	deliver("b-only", "member-b@example.test")
	deliver("imported", "member-a@example.test", "member-b@example.test")
	d, err := holding.Claim(ctx, purgeGateway, "imported", time.Minute)
	check(fx, err)
	check(fx, holding.Acknowledge(ctx, purgeGateway, "imported", d.Lease, d.Digest))
	px.bDigest = fx.orgDigest(fx.b)
	return px
}

// purgeOrgA marks org A deleted and runs the purge until it finishes.
func (px *purgeFixture) purgeOrgA(t *testing.T, kill int) {
	t.Helper()
	setOrg(t, px.db, orgA, "deleted", false, `{}`)
	purgeStepHook = func(step int) error {
		if step == kill {
			return errKilled
		}
		return nil
	}
	defer func() { purgeStepHook = nil }()
	ctx := context.Background()
	if kill != 0 {
		if err := px.srv.runPurges(ctx, time.Now()); !errors.Is(err, errKilled) {
			t.Fatalf("kill at %d: %v", kill, err)
		}
		if live, err := tenancy.LivePurges(ctx, px.db); err != nil || len(live) != 1 {
			t.Fatalf("a killed purge is not live: %v %v", live, err)
		}
	}
	purgeStepHook = nil
	if err := px.srv.runPurges(ctx, time.Now()); err != nil {
		t.Fatalf("resumed purge after a kill at %d: %v", kill, err)
	}
}

// assertPurged checks that nothing of org A remains, that org B is untouched, and that
// nothing of A can come back.
func (px *purgeFixture) assertPurged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	srv := px.srv
	if live, err := tenancy.LivePurges(ctx, px.db); err != nil || len(live) != 0 {
		t.Fatalf("purge still live: %v %v", live, err)
	}
	var done, members int
	if err := px.db.QueryRow(`SELECT count(*) FROM org_purges WHERE org_id = ? AND subject = '' AND done_at IS NOT NULL`, orgA).Scan(&done); err != nil || done != 1 {
		t.Fatalf("org_purges not done: %d %v", done, err)
	}
	if err := px.db.QueryRow(`SELECT count(*) FROM org_purge_members`).Scan(&members); err != nil || members != 0 {
		t.Fatalf("snapshot kept: %d %v", members, err)
	}
	for _, m := range []orgMember{px.a, px.other} {
		if _, err := srv.users.Get(m.ID); !errors.Is(err, users.ErrNotFound) {
			t.Fatalf("%s's account survived: %v", m.Username, err)
		}
		if userOrgRow(t, px.db, m.ID) != "" {
			t.Fatal("user_orgs row survived")
		}
		for _, dir := range []string{config.UserDir(srv.configDir, m.ID), config.UserDir(srv.stateDir, m.ID), filepath.Join(srv.stateDir, "mailboxes", m.extra)} {
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s survived: %v", dir, err)
			}
		}
		if n, err := srv.globalStore.UserRows(ctx, []string{m.ID}); err != nil || n != 0 {
			t.Fatalf("root-store rows survived: %d %v", n, err)
		}
		if n, err := srv.pickupStore.SenderRecords([]string{m.ID}, false); err != nil || n != 0 {
			t.Fatalf("pickup records survived: %d %v", n, err)
		}
	}
	// subject_orgs is kept: it holds opaque IDs and stops the subject joining another org.
	if subjectOrgRow(t, srv, px.db, "member-a") != orgA {
		t.Fatal("subject_orgs row not kept")
	}
	addresses, err := srv.ssoLifecycle.NativeAddresses()
	if err != nil {
		t.Fatal("ledger does not open after the purge:", err)
	}
	for _, a := range px.aAddress {
		if _, ok := addresses[a]; ok {
			t.Fatalf("ledger still holds %s", a)
		}
	}
	if _, ok := addresses["member-b@example.test"]; !ok {
		t.Fatal("org B's address was purged")
	}
	holding, err := ingress.OpenExisting(filepath.Join(srv.stateDir, "receiving"), ingress.ReceivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer holding.Close()
	if d, err := holding.Get(ctx, purgeGateway, "a-only"); err != nil || d.Disposition != "discarded" || len(d.Raw) != 0 {
		t.Fatalf("org A's own delivery: %+v %v", d.Disposition, err)
	}
	for _, id := range []string{"shared", "b-only"} {
		d, err := holding.Get(ctx, purgeGateway, id)
		if err != nil || len(d.Bindings) != 1 || d.Bindings[0].Mailbox != px.b.ID || len(d.Raw) == 0 {
			t.Fatalf("org B's delivery %s: %+v %v", id, d.Bindings, err)
		}
	}
	if left, err := holding.PurgeLeft(ctx, []string{px.a.ID, px.a.extra, px.other.ID, px.other.extra}, func(a string) (bool, error) { return slices.Contains(px.aAddress, a), nil }); err != nil || left != 0 {
		t.Fatalf("receiving still names org A: %d %v", left, err)
	}
	if after := px.orgDigest(px.b); after != px.bDigest {
		t.Fatalf("org B changed:\n%s", orgDigestDiff(px.bDigest, after))
	}
	if w := px.call(px.b, http.MethodGet, "/api/contacts", "", nil); w.Code != http.StatusOK {
		t.Fatal("org B's member lost access", w.Code)
	}
	// Tombstones refuse reuse: as an alias anywhere, and as a new member's primary.
	for _, a := range px.aAddress {
		if _, err := srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, px.b.extra, a); !errors.Is(err, sso.ErrNativeAddressConflict) {
			t.Fatalf("tombstoned %s reissued: %v", a, err)
		}
	}
	// The purged subject is never provisioned again.
	u := orgUser("member-a", orgA, true)
	u["emails"] = []map[string]any{{"value": "member-a@example.test", "primary": true}}
	postDirectory(t, srv, testSyncKey, "user.updated", "ev-again", 9, u)
	if _, err := srv.users.GetBySSOSub("member-a"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("a purged subject was provisioned again", err)
	}
	// Nor does a new subject of the deleted org get an account.
	fresh := orgUser("member-a3", orgA, true)
	fresh["emails"] = []map[string]any{{"value": "member-a3@example.test", "primary": true}}
	postDirectory(t, srv, testSyncKey, "user.created", "ev-fresh", 1, fresh)
	if _, err := srv.users.GetBySSOSub("member-a3"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("a deleted org's new subject was provisioned", err)
	}
	// A second pass changes nothing.
	if err := srv.runPurges(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestOrgPurgeResumesFromEveryStep kills the purge before and after each step, then lets
// it resume: it always finishes, and nothing of the org remains (K PR 6 tests).
func TestOrgPurgeResumesFromEveryStep(t *testing.T) {
	kills := []int{0}
	for step := 1; step <= 5; step++ {
		kills = append(kills, step, -step)
	}
	for _, kill := range kills {
		t.Run(fmt.Sprint("kill=", kill), func(t *testing.T) {
			px := newPurgeFixture(t)
			px.purgeOrgA(t, kill)
			px.assertPurged(t)
		})
	}
}

// TestOrgPurgeFinalCheckRefusesLeftovers recreates a member's directory after the last
// step: the final check refuses to mark the purge done, and the next pass removes it.
func TestOrgPurgeFinalCheckRefusesLeftovers(t *testing.T) {
	px := newPurgeFixture(t)
	setOrg(t, px.db, orgA, "deleted", false, `{}`)
	dir := config.UserDir(px.srv.configDir, px.a.ID)
	purgeStepHook = func(step int) error {
		if step == -5 {
			return os.MkdirAll(dir, 0o700)
		}
		return nil
	}
	defer func() { purgeStepHook = nil }()
	if err := px.srv.runPurges(context.Background(), time.Now()); !errors.Is(err, errPurgeLeft) || !strings.Contains(err.Error(), "directories: 1") {
		t.Fatal("final check passed with a leftover directory:", err)
	}
	purgeStepHook = nil
	if err := px.srv.runPurges(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	px.assertPurged(t)
}

// TestOrgPurgeRefusals: the staff org is never purged, the ledger exception needs a live
// purge row, a user_orgs row naming another org's account stops the snapshot, and missing
// tombstone key fails closed.
func TestOrgPurgeRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("staff org", func(t *testing.T) {
		px := newPurgeFixture(t)
		setOrg(t, px.db, staffOrg, "deleted", false, `{}`)
		if err := px.srv.runPurges(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if done, err := tenancy.Purged(ctx, px.db, staffOrg, ""); err != nil || done {
			t.Fatal("the staff org was purged", err)
		}
	})
	t.Run("no live row", func(t *testing.T) {
		px := newPurgeFixture(t)
		owners := []sso.PurgeOwner{{Issuer: px.srv.ssoStore.Load().IssuerURL, Subject: "member-a"}}
		err := px.srv.ssoLifecycle.PurgeNative(ctx, owners, func() error { return tenancy.PurgeLive(ctx, px.db, tenancy.PurgeScope{Org: orgA}) }, func([]string) error { return nil })
		if !errors.Is(err, tenancy.ErrNoLivePurge) {
			t.Fatal("ledger scrubbed without a live purge:", err)
		}
		if addresses, err := px.srv.ssoLifecycle.NativeAddresses(); err != nil || addresses["member-a@example.test"].Mailbox == "" {
			t.Fatal("refused exception changed the ledger", err)
		}
	})
	t.Run("another org's account", func(t *testing.T) {
		px := newPurgeFixture(t)
		if _, err := px.db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, px.b.ID); err != nil {
			t.Fatal(err)
		}
		if err := tenancy.BindUser(ctx, px.db, px.b.ID, orgA); err != nil {
			t.Fatal(err)
		}
		setOrg(t, px.db, orgA, "deleted", false, `{}`)
		if err := px.srv.runPurges(ctx, time.Now()); err == nil {
			t.Fatal("a snapshot took another org's account")
		}
		if _, err := px.srv.users.Get(px.b.ID); err != nil {
			t.Fatal("org B's account was removed", err)
		}
	})
	t.Run("tombstone key lost", func(t *testing.T) {
		px := newPurgeFixture(t)
		px.purgeOrgA(t, 0)
		if err := os.Remove(filepath.Join(config.SecretDir(), tenancy.TombstoneKeyFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := px.srv.ssoLifecycle.AddNativeAlias(ctx, px.srv.stateDir, px.b.extra, "fresh@example.test"); !errors.Is(err, tenancy.ErrTombstoneKeyMissing) {
			t.Fatal("allocation without the tombstone key:", err)
		}
	})
}

// TestOrgPurgeKeepsTheLedgerTriggers: after the named exception the never-delete triggers
// are back, so a plain delete of org B's address is still refused.
func TestOrgPurgeKeepsTheLedgerTriggers(t *testing.T) {
	px := newPurgeFixture(t)
	px.purgeOrgA(t, 0)
	err := px.srv.ssoLifecycle.PurgeNative(context.Background(), []sso.PurgeOwner{{Issuer: px.srv.ssoStore.Load().IssuerURL, Subject: "member-b"}},
		func() error { return nil }, func([]string) error { return errKilled })
	if !errors.Is(err, errKilled) {
		t.Fatal(err)
	}
	if addresses, err := px.srv.ssoLifecycle.NativeAddresses(); err != nil || addresses["member-b@example.test"].Mailbox == "" {
		t.Fatal("a failed tombstone write still scrubbed the ledger", err)
	}
	if dbbackend.Postgres() {
		return // the schema fingerprint check at every open covers the triggers there
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(px.srv.configDir, sso.NativeAuthorityFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM addresses WHERE address = 'member-b@example.test'`); err == nil || !strings.Contains(err.Error(), "never deleted") {
		t.Fatal("the never-delete trigger is gone:", err)
	}
}

// TestOrgPurgeRunsAgainAfterARestore: a restore forgets the cache and finished purges
// (tenancy.FenceRestored). The next poll lists the org as deleted again, the purge runs
// again over what the restored ledger still names, its final check finds nothing, and the
// restored tombstones, under the same key, still refuse every address.
func TestOrgPurgeRunsAgainAfterARestore(t *testing.T) {
	px := newPurgeFixture(t)
	px.purgeOrgA(t, 0)
	ctx := context.Background()
	if err := tenancy.ForgetRestored(ctx, px.db); err != nil {
		t.Fatal(err)
	}
	if w := px.call(px.b, http.MethodGet, "/api/contacts", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatal("an org was served before a poll after the restore", w.Code)
	}
	setOrg(t, px.db, orgB, "active", true, `{}`)
	px.bDigest = px.orgDigest(px.b)
	px.purgeOrgA(t, 0)
	px.assertPurged(t)
}

// TestPurgeFinalCheckCoversTheClassification: every org-owned and derived store is mapped
// (tenancy.TestPurgeCoversEveryOrgStore) to a part the final check runs and reports.
func TestPurgeFinalCheckCoversTheClassification(t *testing.T) {
	px := newPurgeFixture(t)
	left, err := px.srv.purgeLeft(context.Background(), tenancy.PurgeSnapshot{}, func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	for store, check := range tenancy.PurgeCoverage {
		if _, ok := left[check]; !ok && check != "kept" {
			t.Errorf("%s is covered by %q, which the final check does not run", store, check)
		}
	}
	if len(left) != len(purgeChecks) {
		t.Errorf("the final check reports %v, want exactly %v", left, purgeChecks)
	}
}
