//go:build linux

package sso

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// answerEveryDomain serves each configured domain's own record unless lapsed.
func answerEveryDomain(s *NativeDomainStore, lapsed map[string]bool) {
	s.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		set, err := s.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." && !lapsed[d.Domain] {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, err
	})
}

func TestNativeDomainSetFencesOnlyTheTouchedDomain(t *testing.T) {
	ctx := context.Background()
	s := NewNativeDomainStore(t.TempDir())
	if _, err := s.Configure(ctx, "example.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigureDomain(ctx, "second.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigureDomain(ctx, "third.test", "https://other.test"); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("second issuer joined the set", err)
	}
	if _, err := s.Configure(ctx, "second.test", nativeIssuer); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("single-domain API left the founding domain", err)
	}
	if d, err := s.Read(); err != nil || d.Domain != "example.test" {
		t.Fatal("founding domain", d, err)
	}
	lapsed := map[string]bool{}
	answerEveryDomain(s, lapsed)
	a, err := s.VerifyDomain(ctx, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyDomain(ctx, "second.test"); err != nil {
		t.Fatal(err)
	}
	// Re-challenging B fences B only.
	b, err := s.ConfigureDomain(ctx, "second.test", nativeIssuer)
	if err != nil {
		t.Fatal(err)
	}
	set, err := s.ReadSet()
	if err != nil || !set.CurrentProof(a) || set.CurrentProof(b) {
		t.Fatal("re-verifying one domain fenced another", err)
	}
	lapsed["second.test"] = true
	if _, err = s.VerifyDomain(ctx, "second.test"); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("lapsed domain verified", err)
	}
	if _, err = s.VerifyDomain(ctx, "example.test"); err != nil {
		t.Fatal("lapse of one domain suspended another", err)
	}
	if _, err = s.VerifyDomain(ctx, "unknown.test"); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("unconfigured domain verified", err)
	}
}

func TestNativeTwoDomainsAllocateLapseAndRetire(t *testing.T) {
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	if _, err := domains.ConfigureDomain(ctx, "second.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	lapsed := map[string]bool{}
	answerEveryDomain(domains, lapsed)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	allocate := func(sub, address string) (users.User, error) {
		nativeDesired(t, life, sub, address, 1, true)
		return life.AllocateNativeAccount(ctx, root, nativeIssuer, sub, domains, accounts, nativeLimits)
	}
	if _, err = allocate("one", "one@example.test"); err != nil {
		t.Fatal(err)
	}
	two, err := allocate("two", "Two@Second.test")
	if err != nil {
		t.Fatal(err)
	}
	if a, _, _ := life.NativeAssignment(nativeIssuer, "two"); a.Address != "two@second.test" {
		t.Fatal("primary on the second domain", a.Address)
	}
	if _, err = allocate("stray", "stray@unknown.test"); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("primary on an unconfigured domain", err)
	}
	lapsed["second.test"] = true
	if _, err = allocate("three", "three@second.test"); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("lapsed domain allocated", err)
	}
	if _, err = allocate("four", "four@example.test"); err != nil {
		t.Fatal("lapse of one domain blocked another", err)
	}
	lapsed["second.test"] = false
	relayKey := filepath.Join(t.TempDir(), "native-relay.key")
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); !errors.Is(err, ErrNativeDomainInUse) {
		t.Fatal("retired a domain with an active address", err)
	}
	nativeDesired(t, life, "two", "two@second.test", 2, false)
	if _, err = life.ReconcileNativeMailbox(root, nativeIssuer, "two", two.ID, "second.test", nativeLimits); err != nil {
		t.Fatal(err)
	}
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); err != nil {
		t.Fatal(err)
	}
	set, err := domains.ReadSet()
	if err != nil || !slices.Equal(set.Retired, []string{"second.test"}) || set.Founding != "example.test" {
		t.Fatal("retirement", set, err)
	}
	if a, ok, _ := life.NativeAssignment(nativeIssuer, "two"); !ok || a.Address != "two@second.test" {
		t.Fatal("retirement dropped the address record", a)
	}
	if _, err = domains.ConfigureDomain(ctx, "second.test", nativeIssuer); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("retired domain re-added", err)
	}
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("retired twice", err)
	}
	// Reactivation in the directory does not reopen a retired domain.
	nativeDesired(t, life, "two", "two@second.test", 3, true)
	if err = life.WithNativeMailAccess(ctx, root, nativeIssuer, accounts, []string{two.ID}, func(map[string]NativeAssignment) error { return nil }); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("retired domain admitted", err)
	}
	list, err := accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = life.ValidateNativeSnapshot(root, list); err != nil {
		t.Fatal("snapshot with a retired domain refused", err)
	}
	// An address on a domain neither configured nor retired is refused.
	var raw map[string]any
	if err = json.Unmarshal(mustRead(t, domains.path), &raw); err != nil {
		t.Fatal(err)
	}
	raw["retired"] = []string{}
	if err = fsutil.PersistJSONFile(domains.path, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = life.ValidateNativeSnapshot(root, list); err == nil {
		t.Fatal("snapshot with an address on an unknown domain accepted")
	}
}

func TestNativeRetirementWaitsForQueuedOutbox(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, job); err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(sender.ConfigDir)
	nativeDesired(t, life, "one", "one@example.test", 2, false)
	if _, err = life.ReconcileNativeMailbox(sender.StateRoot, nativeIssuer, "one", u.ID, "example.test", nativeLimits); err != nil {
		t.Fatal(err)
	}
	if err = sender.Domains.RetireDomain(ctx, "example.test", sender.StateRoot, sender.keyPath()); !errors.Is(err, ErrNativeDomainInUse) {
		t.Fatal("retired a domain with a queued job", err)
	}
	if err = sender.Quarantine(ctx, u.ID, id, 0); err != nil {
		t.Fatal(err)
	}
	if err = sender.Domains.RetireDomain(ctx, "example.test", sender.StateRoot, sender.keyPath()); err != nil {
		t.Fatal(err)
	}
	// With no domain left the single-domain API reads as unconfigured.
	if d, err := sender.Domains.Read(); err != nil || d.Domain != "" {
		t.Fatal("founding after retirement", d, err)
	}
}

func TestNativeOutboundFromMustBeARelayDomain(t *testing.T) {
	ctx := context.Background()
	sender, _, _ := outboundFixture(t)
	if _, err := sender.Domains.ConfigureDomain(ctx, "second.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	answerEveryDomain(sender.Domains, nil)
	life := NewLifecycleStore(sender.ConfigDir)
	nativeDesired(t, life, "two", "two@second.test", 1, true)
	two, err := life.AllocateNativeAccount(ctx, sender.StateRoot, nativeIssuer, "two", sender.Domains, sender.Accounts, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: two@second.test\r\nTo: recipient@example.test\r\nSubject: retained\r\n\r\nmessage\r\n")
	job := mailbox.OutboundJob{From: "two@second.test", NativeSendEpoch: two.NativeSendEpoch, PGPRevision: two.PGPRevision, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"recipient@example.test"}, Raw: raw}}}
	queue := func() error {
		id, err := fsutil.NewUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		return sender.Queue(ctx, two.ID, id, job)
	}
	if err = queue(); !errors.Is(err, mailmsg.ErrDomainRelay) {
		t.Fatal("From outside the relay set queued", err)
	}
	path := filepath.Join(sender.ConfigDir, "native-relay.json")
	before, _, err := mailmsg.ReadDomainRelay(path, sender.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	after, err := mailmsg.SetDomainRelayDomains(ctx, path, sender.keyPath(), []string{"second.test", "example.test"})
	if err != nil || after.Generation != before.Generation || !slices.Equal(after.Domains, []string{"example.test", "second.test"}) {
		t.Fatal("adding a relay domain changed the generation", after, err)
	}
	if err = queue(); err != nil {
		t.Fatal("From on an added relay domain refused", err)
	}
	removed, err := mailmsg.SetDomainRelayDomains(ctx, path, sender.keyPath(), []string{"example.test"})
	if err != nil || removed.Generation != before.Generation || !slices.Equal(removed.RetiredDomains, []string{"second.test"}) {
		t.Fatal("removed relay domain not retired", removed, err)
	}
	if err = queue(); !errors.Is(err, mailmsg.ErrDomainRelay) {
		t.Fatal("From on a removed relay domain queued", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = mailmsg.SetDomainRelayDomains(ctx, path, sender.keyPath(), []string{"example.test"}); !errors.Is(err, mailmsg.ErrDomainRelay) {
		t.Fatal("domain-only update created a relay", err)
	}
}
