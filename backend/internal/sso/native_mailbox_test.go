//go:build linux

package sso

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func (f addressFixture) admit(id, mailboxID string) error {
	_, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, mailboxID, f.accounts)
	return err
}

func TestNativeExtraMailboxCreateRefusals(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one := f.ids["one"]
	if _, err := f.life.AddNativeAlias(ctx, f.root, one, "alias@example.test"); err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, f.life, "pending", "pending@example.test", 1, true)
	for _, tc := range []struct {
		user, address string
		want          error
	}{
		{one, "not-an-address", ErrNativeAddressInvalid},
		{"no-such-user", "sales@example.test", ErrNativeAddressUnknown},
		{one, "two@example.test", ErrNativeAddressDirectory},
		{one, "one@example.test", ErrNativeAddressDirectory},
		{one, "alias@example.test", ErrNativeAddressConflict},
		{one, "sales@unknown.test", ErrNativeAddressDomain},
		// Another subject's KyIdentity primary, before it has a ledger record.
		{one, "pending@example.test", ErrNativeAddressDirectory},
	} {
		if _, err := f.life.CreateNativeMailbox(ctx, f.root, tc.user, tc.address); !errors.Is(err, tc.want) {
			t.Fatalf("create %s for %s: got %v want %v", tc.address, tc.user, err, tc.want)
		}
	}
	nativeRole(t, f.life, f.accounts, "two", "two@example.test", 2, true, true)
	if _, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["two"], "boss@example.test"); !errors.Is(err, ErrNativeAddressAdministrator) {
		t.Fatal("administrator gained an extra mailbox", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.root, nativeMailboxesDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
		t.Fatal("refused creation left storage", entries, err)
	}
}

func TestNativeLegacyMixedUseGainsNoExtraMailbox(t *testing.T) {
	config, keyPath, ids := v1Fixture(t)
	if _, err := MigrateNative(context.Background(), config, keyPath); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	a, _, err := life.NativeAssignment(nativeIssuer, "boss")
	if err != nil || !a.LegacyMixedUse {
		t.Fatal("fixture", a, err)
	}
	if _, err := life.CreateNativeMailbox(context.Background(), a.StateRoot, ids["boss"], "boss-extra@example.test"); !errors.Is(err, ErrNativeAddressAdministrator) {
		t.Fatal("legacyMixedUse subject gained an extra mailbox", err)
	}
}

func TestNativeExtraMailboxLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one, two := f.ids["one"], f.ids["two"]
	m, err := f.life.CreateNativeMailbox(ctx, f.root, one, "Sales@Example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.ID, extraMailboxPrefix) || m.User != one || m.Kind != "extra" || m.State != "active" || len(m.Addresses) != 1 {
		t.Fatalf("created %+v", m)
	}
	if x := m.Addresses[0]; x.Address != "sales@example.test" || x.Kind != "primary" || x.State != "active" || x.Generation != 2 {
		t.Fatalf("extra primary %+v", x)
	}
	if _, err := os.Lstat(filepath.Join(f.root, nativeMailboxesDir, m.ID, "native-mailbox.json")); err != nil {
		t.Fatal("extra storage not under $STATE/mailboxes", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "users", m.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("extra mailbox stored as a user", err)
	}
	// The address of another mailbox and a second create are both refused.
	if _, err := f.life.CreateNativeMailbox(ctx, f.root, two, "sales@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("address taken twice", err)
	}

	a, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, one, m.ID, f.accounts)
	if err != nil || a.Owner.Mailbox != m.ID || a.UserID() != one || a.Address != "sales@example.test" || a.Dir(f.root) != filepath.Join(f.root, nativeMailboxesDir, m.ID) {
		t.Fatalf("owner refused its extra mailbox %+v %v", a, err)
	}
	primary, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, one, one, f.accounts)
	if err != nil || primary.Owner.Mailbox != one || primary.Dir(f.root) != filepath.Join(f.root, "users", one) {
		t.Fatal("primary", primary, err)
	}
	for _, tc := range []struct{ user, mailbox string }{{two, m.ID}, {one, "mbx-unknown"}, {one, two}} {
		if err := f.admit(tc.user, tc.mailbox); !errors.Is(err, ErrNativeMailboxUnknown) {
			t.Fatal("foreign or unknown mailbox admitted", tc, err)
		}
	}
	access := func(ids ...string) (map[string]NativeAssignment, error) {
		var got map[string]NativeAssignment
		err := f.life.WithNativeMailAccess(ctx, f.root, nativeIssuer, f.accounts, ids, func(current map[string]NativeAssignment) error { got = current; return nil })
		return got, err
	}
	if got, err := access(m.ID, one); err != nil || got[m.ID].Owner.Mailbox != m.ID || got[one].Owner.Mailbox != one {
		t.Fatal("receiving authority by mailbox", got, err)
	}

	// Disable bumps the address generation, refuses access and is not repeated.
	disabled, err := f.life.SetNativeMailboxState(ctx, f.root, m.ID, false)
	if err != nil || disabled.State != "disabled" || disabled.Addresses[0].State != "disabled" || disabled.Addresses[0].Generation != 3 {
		t.Fatalf("disable %+v %v", disabled, err)
	}
	if _, err := f.life.SetNativeMailboxState(ctx, f.root, m.ID, false); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("disabled twice", err)
	}
	if _, err := f.life.SetNativeMailboxState(ctx, f.root, one, false); !errors.Is(err, ErrNativeAddressUnknown) {
		t.Fatal("primary mailbox disabled by administrator", err)
	}
	if err := f.admit(one, m.ID); !errors.Is(err, ErrNativeMailboxUnknown) {
		t.Fatal("disabled mailbox admitted", err)
	}
	if _, err := access(m.ID); !errors.Is(err, ErrNativeMailboxUnknown) {
		t.Fatal("disabled mailbox receives", err)
	}
	// A directory edit leaves the disabled address alone (level-triggered).
	nativeDesired(t, f.life, "one", "one@example.test", 2, true)
	if x := f.address(t, "sales@example.test"); x.State != "disabled" || x.Generation != 3 {
		t.Fatal("directory edit re-enabled an administrator-disabled mailbox", x)
	}
	enabled, err := f.life.SetNativeMailboxState(ctx, f.root, m.ID, true)
	if err != nil || enabled.State != "active" || enabled.Addresses[0].State != "active" || enabled.Addresses[0].Generation != 4 {
		t.Fatalf("enable %+v %v", enabled, err)
	}
	if err := f.admit(one, m.ID); err != nil {
		t.Fatal("re-enabled mailbox refused", err)
	}

	// Subject deactivation disables every mailbox; reactivation restores them.
	nativeDesired(t, f.life, "one", "one@example.test", 3, false)
	if x := f.address(t, "sales@example.test"); x.State != "disabled" || x.Generation != 5 {
		t.Fatal("deactivation left the extra mailbox address active", x)
	}
	if err := f.admit(one, m.ID); err == nil {
		t.Fatal("deactivated subject admitted to its extra mailbox")
	}
	nativeDesired(t, f.life, "one", "one@example.test", 4, true)
	if x := f.address(t, "sales@example.test"); x.State != "active" || x.Generation != 6 {
		t.Fatal("reactivation", x)
	}
	// Promotion disables it like the primary.
	nativeRole(t, f.life, f.accounts, "one", "one@example.test", 5, true, true)
	if x := f.address(t, "sales@example.test"); x.State != "disabled" {
		t.Fatal("promotion left the extra mailbox address active", x)
	}
}

// A creation killed after its reservation resumes with the same address and ID.
func TestNativeExtraMailboxCreationResumes(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	m, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["one"], "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := f.life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.stored.Mailboxes[m.ID]
	record.Source = ""
	ledger.stored.Mailboxes[m.ID] = record
	if err := f.life.commitNative(ledger, nil); err != nil {
		t.Fatal(err)
	}
	if x := f.address(t, "sales@example.test"); x.State != "disabled" {
		t.Fatal("unprepared extra mailbox routes", x)
	}
	if err := f.admit(f.ids["one"], m.ID); !errors.Is(err, ErrNativeMailboxUnknown) {
		t.Fatal("unprepared extra mailbox admitted", err)
	}
	if _, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["two"], "sales@example.test"); !errors.Is(err, ErrNativeAddressConflict) {
		t.Fatal("another owner took over an unfinished creation", err)
	}
	// Killed before its storage was published: a backup still validates.
	if err := os.RemoveAll(filepath.Join(f.root, nativeMailboxesDir, m.ID)); err != nil {
		t.Fatal(err)
	}
	list, err := f.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); err != nil {
		t.Fatal("reserved, unprepared extra mailbox refused", err)
	}
	resumed, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["one"], "sales@example.test")
	if err != nil || resumed.ID != m.ID || resumed.Addresses[0].State != "active" {
		t.Fatal("resume", resumed, err)
	}
}

func TestNativeExtraMailboxSnapshotValidation(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	m, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["one"], "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); err != nil {
		t.Fatal("snapshot with an extra mailbox refused", err)
	}
	mailboxes, err := f.life.NativeRestoreMailboxes(f.root)
	if err != nil || mailboxes[m.ID].Source == "" || mailboxes[m.ID].Primary || mailboxes[m.ID].Dir != filepath.Join(f.root, nativeMailboxesDir, m.ID) || !mailboxes[f.ids["one"]].Primary {
		t.Fatal("restore mailboxes", mailboxes, err)
	}
	extraState, err := state.OpenNative(mailboxes[m.ID].Dir, mailboxes[m.ID].Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := extraState.UpsertNativeDevice(state.NativeDevice{DeviceID: "phone", Platform: "android", PushToken: "push", SecretHash: "credential"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("device row in an extra mailbox accepted", err)
	}
	if _, err := extraState.RemoveNativeDevice("phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := extraState.GetOrCreateSubscriberID(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("subscriber in an extra mailbox accepted", err)
	}
	_ = extraState.Close()
	db, err := sql.Open("sqlite", filepath.Join(f.root, nativeMailboxesDir, m.ID, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("DELETE FROM meta WHERE key='subscriber_id'")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); err != nil {
		t.Fatal("cleaned extra mailbox refused", err)
	}
	// A primary's storage copied under $STATE/mailboxes is misplaced.
	primaryCopy := filepath.Join(f.root, nativeMailboxesDir, f.ids["one"])
	if err := exec.Command("cp", "-a", filepath.Join(f.root, "users", f.ids["one"]), primaryCopy).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("primary storage under mailboxes accepted", err)
	}
	if err := os.RemoveAll(primaryCopy); err != nil {
		t.Fatal(err)
	}
	// An extra mailbox's storage under $STATE/users, or an unknown one under
	// $STATE/mailboxes, is an orphan.
	extra := filepath.Join(f.root, nativeMailboxesDir, m.ID)
	moved := filepath.Join(f.root, "users", m.ID)
	if err := os.Rename(extra, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("extra mailbox placed under users accepted", err)
	}
	if err := os.Rename(moved, filepath.Join(f.root, nativeMailboxesDir, "mbx-orphan")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, list); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("orphan extra mailbox accepted", err)
	}
}

func TestNativeRecoveryDigestIncludesExtraMailboxes(t *testing.T) {
	life, dir, settings, key, challenge := recoveryFixture(t)
	f, err := life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	m := nativeLedgerMailbox{Kind: "extra", State: "disabled", StateRoot: dir, Limits: nativeLimits}
	m.Owner.Issuer, m.Owner.Subject = settings.IssuerURL, "subject"
	f.stored.Mailboxes["mbx-extra"] = m
	f.stored.Addresses["extra@example.test"] = nativeLedgerAddress{Mailbox: "mbx-extra", Kind: "primary", State: "disabled", Generation: 1, History: []nativeAddressHistory{{"mbx-extra", 1}}}
	if err := life.saveNative(f); err != nil {
		t.Fatal(err)
	}
	if _, err := life.loadNative(); err != nil {
		t.Fatal("ledger with an extra mailbox unreadable", err)
	}
	digest := func() string {
		c, _, _, err := life.nativeRecoveryInputs(dir, settings, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.AuthorityDigest
	}
	disabled := digest()
	// The mailbox state alone is authority: re-enabling changes the digest.
	f, err = life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	m.State = "active"
	f.stored.Mailboxes["mbx-extra"] = m
	if err := life.saveNative(f); err != nil {
		t.Fatal(err)
	}
	if digest() == disabled {
		t.Fatal("extra mailbox state missing from the recovery digest")
	}
	without, err := life.loadNative()
	if err != nil {
		t.Fatal(err)
	}
	delete(without.stored.Mailboxes, "mbx-extra")
	delete(without.stored.Addresses, "extra@example.test")
	if err := life.saveNative(without); err != nil {
		t.Fatal(err)
	}
	if digest() != challenge.AuthorityDigest {
		t.Fatal("a ledger without extra mailboxes must keep earlier digests valid")
	}
}

// An extra mailbox sends only from its own active addresses, keeps its own
// outbox and files Sent in itself; the primary cannot borrow its address.
func TestNativeOutboundFromExtraMailbox(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	life := NewLifecycleStore(sender.ConfigDir)
	m, err := life.CreateNativeMailbox(ctx, sender.StateRoot, u.ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	extraJob := aliasJob(job)
	extraJob.Sent = extraJob.Deliveries[0].Raw
	queue := func(mailboxID string, job mailbox.OutboundJob) (string, error) {
		id, err := fsutil.NewUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		return id, sender.Queue(ctx, mailboxID, id, job)
	}
	if _, err := queue(u.ID, extraJob); !errors.Is(err, ErrNativeOutboundStale) {
		t.Fatal("primary sent from an extra mailbox's address", err)
	}
	if _, err := queue(m.ID, job); !errors.Is(err, ErrNativeOutboundStale) {
		t.Fatal("extra mailbox sent from the primary address", err)
	}
	id, err := queue(m.ID, extraJob)
	if err != nil {
		t.Fatal("extra mailbox refused its own address", err)
	}
	box, key, err := sender.openStorage(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, _, err := box.ReadOutbound(ctx, key, id)
	_ = box.Close()
	if err != nil || stored.From != "sales@example.test" || stored.FromGeneration != 2 {
		t.Fatal("extra outbox job", stored.From, stored.FromGeneration, err)
	}
	if _, _, err := sender.Status(ctx, u.ID, id); err == nil {
		t.Fatal("extra mailbox job visible in the primary outbox")
	}
	if ids, err := sender.Pending(ctx, m.ID, 10); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatal("extra outbox pending", ids, err)
	}
	// A device-bound job is fenced on the device in the owner's primary state.
	devices, err := state.OpenNative(filepath.Join(sender.StateRoot, "users", u.ID), u.NativeMailboxSource)
	if err != nil {
		t.Fatal(err)
	}
	device := state.NativeDevice{DeviceID: "phone", SecretHash: "credential", Platform: "android", PushToken: "push"}
	err = devices.UpsertNativeDevice(device)
	_ = devices.Close()
	if err != nil {
		t.Fatal(err)
	}
	deviceJob := extraJob
	deviceJob.DeviceID, deviceJob.DeviceWitness = device.DeviceID, state.NativeDeviceWitness(device)
	if _, err := queue(m.ID, deviceJob); err != nil {
		t.Fatal("device-bound send from an extra mailbox refused", err)
	}
	// As if the relay accepted it: Sent is filed in the extra mailbox only.
	sentCount := func(dir string) int {
		db, err := sql.Open("sqlite", filepath.Join(dir, "mailbox", "mailbox.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow("SELECT count(*) FROM messages WHERE folder='Sent'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	extraDir := filepath.Join(sender.StateRoot, nativeMailboxesDir, m.ID)
	relay, _, err := mailmsg.ReadDomainRelay(filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	box, key, err = sender.openStorage(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, claim, err := box.ClaimOutbound(ctx, key, id, 0, relay.Generation)
	if err == nil {
		err = box.CompleteOutbound(ctx, id, 0, claim, nil)
	}
	_ = box.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.FileSent(ctx, m.ID, id); err != nil {
		t.Fatal(err)
	}
	if sentCount(extraDir) != 1 || sentCount(filepath.Join(sender.StateRoot, "users", u.ID)) != 0 {
		t.Fatal("Sent not filed in the sending mailbox only")
	}
	// A disabled mailbox sends nothing; its outbox stays readable.
	if _, err := life.SetNativeMailboxState(ctx, sender.StateRoot, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := queue(m.ID, extraJob); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("disabled mailbox sent", err)
	}
	if statuses, sent, err := sender.Status(ctx, m.ID, id); err != nil || len(statuses) != 1 || !sent {
		t.Fatal("disabled mailbox outbox unreadable", statuses, sent, err)
	}
}
