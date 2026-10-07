//go:build linux

package sso

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type addressFixture struct {
	config, root string
	life         *LifecycleStore
	domains      *NativeDomainStore
	accounts     *users.Store
	ids          map[string]string
}

func newAddressFixture(t *testing.T, subjects ...string) addressFixture {
	t.Helper()
	ctx := context.Background()
	f := addressFixture{config: t.TempDir(), root: t.TempDir(), ids: map[string]string{}}
	f.life = NewLifecycleStore(f.config)
	f.domains = provenNativeDomain(t, f.config)
	var err error
	if f.accounts, err = users.LoadOrMigrate(ctx, f.config, filepath.Join(f.config, "admin.env")); err != nil {
		t.Fatal(err)
	}
	for _, sub := range subjects {
		nativeDesired(t, f.life, sub, sub+"@example.test", 1, true)
		u, err := f.life.AllocateNativeAccount(ctx, f.root, nativeIssuer, sub, f.domains, f.accounts, nativeLimits)
		if err != nil {
			t.Fatal(err)
		}
		f.ids[sub] = u.ID
	}
	return f
}

func (f addressFixture) address(t *testing.T, address string) NativeAddress {
	t.Helper()
	addresses, err := f.life.NativeAddresses()
	if err != nil {
		t.Fatal(err)
	}
	return addresses[address]
}

func (f addressFixture) generations(t *testing.T) map[string]int64 {
	t.Helper()
	addresses, err := f.life.NativeAddresses()
	if err != nil {
		t.Fatal(err)
	}
	generations := map[string]int64{}
	for address, x := range addresses {
		generations[address] = x.Generation
	}
	return generations
}

func TestNativeAliasLedgerOperations(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one, two := f.ids["one"], f.ids["two"]
	for _, tc := range []struct {
		mailbox, address string
		want             error
	}{
		{one, "not-an-address", ErrNativeAddressInvalid},
		{one, "Display <x@example.test>", ErrNativeAddressInvalid},
		{one, "two@example.test", ErrNativeAddressConflict},
		{"no-such-mailbox", "sales@example.test", ErrNativeAddressUnknown},
		{one, "sales@unknown.test", ErrNativeAddressDomain},
	} {
		if _, err := f.life.AddNativeAlias(ctx, f.root, tc.mailbox, tc.address); !errors.Is(err, tc.want) {
			t.Fatalf("add %s to %s: got %v want %v", tc.address, tc.mailbox, err, tc.want)
		}
	}
	added, err := f.life.AddNativeAlias(ctx, f.root, one, "Sales@Example.test")
	if err != nil || added != (NativeAddress{Address: "sales@example.test", Mailbox: one, Kind: "alias", State: "active", Generation: 1}) {
		t.Fatal("add", added, err)
	}
	if _, err := f.life.AddNativeAlias(ctx, f.root, two, "sales@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("alias taken twice", err)
	}
	// A KyIdentity primary can never claim an alias of another mailbox.
	nativeDesired(t, f.life, "three", "sales@example.test", 1, true)
	if _, err := f.life.AllocateNativeAccount(ctx, f.root, nativeIssuer, "three", f.domains, f.accounts, nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("primary took an alias", err)
	}
	// While "three" names sales@ as its KyIdentity primary, no mailbox may take
	// it as an alias; move that primary away so reassignment can proceed.
	nativeDesired(t, f.life, "three", "three@example.test", 2, true)
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "one@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("primary released", err)
	}
	if _, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", two); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("active alias reassigned without release", err)
	}
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "missing@example.test"); !errors.Is(err, ErrNativeAddressUnknown) {
		t.Fatal("unknown released", err)
	}
	released, err := f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test")
	if err != nil || released.State != "reserved" || released.Generation != 2 || released.Mailbox != one {
		t.Fatal("release", released, err)
	}
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("released twice", err)
	}
	if _, err := f.life.AddNativeAlias(ctx, f.root, two, "sales@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("reserved address re-added instead of reassigned", err)
	}
	if _, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", "no-such-mailbox"); !errors.Is(err, ErrNativeAddressUnknown) {
		t.Fatal("reassigned to an unknown mailbox", err)
	}
	moved, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", two)
	if err != nil || moved != (NativeAddress{Address: "sales@example.test", Mailbox: two, Kind: "alias", State: "active", Generation: 3}) {
		t.Fatal("reassign", moved, err)
	}
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	back, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", one)
	if err != nil || back.Generation != 5 || back.Mailbox != one {
		t.Fatal("reassign back", back, err)
	}
	ledger, err := f.life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	x := ledger.stored.Addresses["sales@example.test"]
	if want := []nativeAddressHistory{{one, 1}, {two, 3}, {one, 5}}; !slices.Equal(x.History, want) {
		t.Fatal("history", x.History)
	}
	for _, c := range []struct {
		mailbox    string
		generation int64
		held       bool
	}{{one, 1, true}, {one, 2, true}, {two, 3, true}, {two, 4, true}, {one, 5, true}, {two, 2, false}, {one, 3, false}, {two, 5, false}, {one, 6, false}, {one, 0, false}} {
		if x.heldBy(c.mailbox, c.generation) != c.held {
			t.Fatalf("heldBy(%s, %d) != %t", c.mailbox, c.generation, c.held)
		}
	}
	mailboxes, err := f.life.NativeMailboxes()
	// "three" keeps the mailbox its failed allocation reserved, with no address.
	if err != nil || len(mailboxes) != 3 {
		t.Fatal(mailboxes, err)
	}
	for _, m := range mailboxes {
		if m.ID == one && (len(m.Addresses) != 2 || m.User != one || m.Addresses[1].Address != "sales@example.test") {
			t.Fatal("listing", m)
		}
	}
}

// Ordinary directory edits never move a generation; deactivation,
// reactivation and promotion each bump inside ApplyDirectory, and a
// legacyMixedUse subject keeps its address active.
func TestNativeAddressGenerationsFollowOnlyStateChanges(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["one"], "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	before := f.generations(t)
	nativeDesired(t, f.life, "one", "one@example.test", 2, true)
	nativeRole(t, f.life, nil, "two", "two@example.test", 2, true, false)
	if _, err := f.life.ReconcileNativeMailbox(f.root, nativeIssuer, "one", f.ids["one"], "example.test", nativeLimits); err != nil {
		t.Fatal(err)
	}
	if after := f.generations(t); !maps.Equal(after, before) {
		t.Fatal("directory edit changed a generation", before, after)
	}
	nativeDesired(t, f.life, "one", "one@example.test", 3, false)
	if x := f.address(t, "sales@example.test"); x.State != "disabled" || x.Generation != 2 {
		t.Fatal("deactivation", x)
	}
	nativeDesired(t, f.life, "one", "one@example.test", 4, true)
	if x := f.address(t, "one@example.test"); x.State != "active" || x.Generation != before["one@example.test"]+2 {
		t.Fatal("reactivation", x)
	}
	nativeRole(t, f.life, nil, "two", "two@example.test", 3, true, true)
	if x := f.address(t, "two@example.test"); x.State != "disabled" || x.Generation != before["two@example.test"]+1 {
		t.Fatal("promotion did not disable", x)
	}
	if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["two"], "boss@example.test"); !errors.Is(err, ErrNativeAddressAdministrator) {
		t.Fatal("alias for an administrator", err)
	}
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", f.ids["two"]); !errors.Is(err, ErrNativeAddressAdministrator) {
		t.Fatal("reassigned to an administrator", err)
	}
	// Reserved stays reserved through any directory state.
	nativeDesired(t, f.life, "one", "one@example.test", 5, true)
	if x := f.address(t, "sales@example.test"); x.State != "reserved" {
		t.Fatal("reserved address changed state", x)
	}
}

func TestNativeLegacyMixedUseGainsNoAlias(t *testing.T) {
	ctx := context.Background()
	config, keyPath, ids := v1Fixture(t)
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	a, _, err := life.NativeAssignment(nativeIssuer, "boss")
	if err != nil || !a.LegacyMixedUse {
		t.Fatal("fixture", a, err)
	}
	if _, err := life.AddNativeAlias(ctx, a.StateRoot, ids["boss"], "boss-alias@example.test"); !errors.Is(err, ErrNativeAddressAdministrator) {
		t.Fatal("legacyMixedUse subject gained an alias", err)
	}
	addresses, err := life.NativeAddresses()
	if err != nil || addresses["boss@example.test"].State != "active" {
		t.Fatal("legacyMixedUse primary must keep working", addresses, err)
	}
}

// A failed ledger write fails the event; the retry converges, and a ledger
// edited behind the rule's back is reconciled by the next commit.
func TestNativeDirectoryReplayConverges(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["one"], "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(f.config, nativeProvisioningFile)
	if err := os.Rename(ledgerPath, ledgerPath+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledgerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":false,"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`
	var u DirectoryUser
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "one-2", Type: "user.updated", At: time.Now()}
	if _, err := f.life.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return true, nil }); err == nil {
		t.Fatal("deactivation recorded over an unwritable ledger")
	}
	if d, _, _ := f.life.Directory(nativeIssuer, "one"); d.Revision != 1 {
		t.Fatal("failed event recorded", d.Revision)
	}
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ledgerPath+".away", ledgerPath); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.life.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return true, nil }); err != nil {
			t.Fatal(err)
		}
		for _, address := range []string{"one@example.test", "sales@example.test"} {
			if x := f.address(t, address); x.State != "disabled" || x.Generation != 2 {
				t.Fatal("replay did not converge once", x)
			}
		}
	}
	// Diverged by hand: the worker's next commit restores the rule.
	ledger, err := f.life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	x := ledger.stored.Addresses["sales@example.test"]
	x.State = "active"
	ledger.stored.Addresses["sales@example.test"] = x
	if err := f.life.saveNative(ledger); err != nil {
		t.Fatal(err)
	}
	if err := f.life.ReconcileNativeAddresses(ctx); err != nil {
		t.Fatal(err)
	}
	if x := f.address(t, "sales@example.test"); x.State != "disabled" || x.Generation != 3 {
		t.Fatal("worker did not reconcile divergence", x)
	}
	stable, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.life.ReconcileNativeAddresses(ctx); err != nil {
		t.Fatal(err)
	}
	// Atomic writes replace the file, so the same inode means no write.
	if again, err := os.Stat(ledgerPath); err != nil || !os.SameFile(again, stable) {
		t.Fatal("a converged ledger was rewritten", err)
	}
}

// Phase-2 ledgers routed on the directory revision; loading raises each
// generation to it once, and later edits leave it alone.
func TestNativeAddressGenerationSeeding(t *testing.T) {
	f := newAddressFixture(t, "one")
	for revision := 2; revision <= 4; revision++ {
		nativeDesired(t, f.life, "one", "one@example.test", revision, true)
	}
	path := filepath.Join(f.config, nativeProvisioningFile)
	var raw map[string]any
	if err := fsutil.LoadJSONFile(path, func(m json.RawMessage) { _ = json.Unmarshal(m, &raw) }, nil); err != nil {
		t.Fatal(err)
	}
	delete(raw, "addressGenerations")
	if err := fsutil.PersistJSONFile(path, raw); err != nil {
		t.Fatal(err)
	}
	if g := f.address(t, "one@example.test").Generation; g != 4 {
		t.Fatal("unseeded generation not raised to the directory revision", g)
	}
	nativeDesired(t, f.life, "one", "one@example.test", 5, true)
	nativeDesired(t, f.life, "one", "one@example.test", 6, true)
	if g := f.address(t, "one@example.test").Generation; g != 4 {
		t.Fatal("seeded generation kept following the directory revision", g)
	}
}

// Restore validates routes and bindings against the address history.
func TestNativeRestoreValidatesAddressHistory(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one, two := f.ids["one"], f.ids["two"]
	if _, err := f.life.AddNativeAlias(ctx, f.root, one, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(f.root, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer holding.Close()
	bind := func(id, mailboxID, subject string, generation int64) {
		t.Helper()
		if err := holding.SetRoute(ctx, ingress.Route{Address: "sales@example.test", Issuer: nativeIssuer, Subject: subject, Mailbox: mailboxID, Generation: generation, Active: true, ValidUntil: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if err := holding.Bind(ctx, "gateway", id, "", "sales@example.test"); err != nil {
			t.Fatal(err)
		}
		if err := holding.Accept(ctx, "gateway", id, "", strings.NewReader("mail "+id)); err != nil {
			t.Fatal(err)
		}
	}
	bind("at-one", one, "one", 1)
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", two); err != nil {
		t.Fatal(err)
	}
	bind("at-two", two, "two", 3)
	list, err := f.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); err != nil {
		t.Fatal("snapshot after release and reassign refused", err)
	}
	// A binding to the first mailbox at the second owner's generation lies
	// outside every history interval.
	bind("outside-history", two, "two", 3)
	db, err := sql.Open("sqlite", filepath.Join(f.root, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE bindings SET mailbox=?,subject='one' WHERE id='outside-history'", one); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("binding outside the address history accepted", err)
	}
}

func TestNativeRecoveryDigestIncludesAddresses(t *testing.T) {
	life, dir, settings, key, challenge := recoveryFixture(t)
	f, err := life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	f.stored.Addresses["held@example.test"] = nativeLedgerAddress{Mailbox: "reserved-mailbox", Kind: "alias", State: "reserved", Generation: 2, History: []nativeAddressHistory{{"reserved-mailbox", 1}}}
	if err := life.saveNative(f); err != nil {
		t.Fatal(err)
	}
	c, _, _, err := life.nativeRecoveryInputs(dir, settings, key, nil)
	if err != nil || c.AuthorityDigest == challenge.AuthorityDigest {
		t.Fatal("addresses missing from the recovery digest", err)
	}
}

// Another subject's KyIdentity primary is never an alias, even before that
// subject has a ledger record (unconfigured domain, administrator, pending).
func TestNativeAliasRefusesAnotherSubjectsDirectoryPrimary(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	nativeDesired(t, f.life, "bee", "Bee@Example.test", 1, true)
	nativeDesired(t, f.life, "away", "away@unconfigured.test", 1, true)
	nativeRole(t, f.life, nil, "boss", "boss@example.test", 1, true, true)
	for _, address := range []string{"bee@example.test", "away@unconfigured.test", "boss@example.test"} {
		if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["one"], address); !errors.Is(err, ErrNativeAddressDirectory) {
			t.Fatal("alias took a directory primary", address, err)
		}
	}
	if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["one"], "spare@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ReleaseNativeAlias(ctx, f.root, "spare@example.test"); err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, f.life, "two", "two@example.test", 1, true)
	if _, err := f.life.AllocateNativeAccount(ctx, f.root, nativeIssuer, "two", f.domains, f.accounts, nativeLimits); err != nil {
		t.Fatal(err)
	}
	// Reassignment checks the same rule: "spare" becomes someone's primary.
	nativeDesired(t, f.life, "two", "spare@example.test", 2, true)
	if _, err := f.life.ReassignNativeAddress(ctx, f.root, "spare@example.test", f.ids["one"]); !errors.Is(err, ErrNativeAddressDirectory) {
		t.Fatal("reassigned onto a directory primary", err)
	}
	if _, err := f.life.AllocateNativeAccount(ctx, f.root, nativeIssuer, "bee", f.domains, f.accounts, nativeLimits); err != nil {
		t.Fatal("subject with a protected primary could not provision", err)
	}
	if x := f.address(t, "bee@example.test"); x.Kind != "primary" || x.State != "active" {
		t.Fatal(x)
	}
}

// A route write that fails after the ledger commit fails the directory event
// (KyIdentity retries) and names the pending route.
func TestNativeDirectoryEventWithPendingRouteIsRetried(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	holding, err := ingress.Open(filepath.Join(f.root, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer holding.Close()
	if err = holding.SetRoute(ctx, ingress.Route{Address: "one@example.test", Issuer: nativeIssuer, Subject: "one", Mailbox: f.ids["one"], Generation: 50, Active: true, ValidUntil: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	raw := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":false,"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`
	var u DirectoryUser
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "one-2", Type: "user.updated", At: time.Now()}
	if _, err := f.life.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return true, nil }); !errors.Is(err, ErrNativeRoutesPending) {
		t.Fatal("route failure not reported", err)
	}
	if d, _, _ := f.life.Directory(nativeIssuer, "one"); d.Revision != 1 {
		t.Fatal("event recorded over a pending route", d.Revision)
	}
	// The ledger never stays ahead of the recorded directory: the worker
	// converges it back (another bump), and KyIdentity's retry redoes the event.
	if err := f.life.ReconcileNativeAddresses(ctx); err != nil {
		t.Fatal(err)
	}
	if x := f.address(t, "one@example.test"); x.State != "active" || x.Generation != 3 {
		t.Fatal("ledger left ahead of the recorded directory", x)
	}
}
