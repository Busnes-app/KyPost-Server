//go:build linux

package sso

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
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
	if _, err = allocate("stray", "stray@unknown.test"); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("primary on an unconfigured domain", err)
	}
	if a, _, _ := life.NativeAssignment(nativeIssuer, "stray"); a.Status != "failed" || a.Failure != "primary_domain_unavailable" {
		t.Fatal("unconfigured primary domain not recorded as a durable failure", a)
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
	// The relay, held incoming mail and a restore hold each block retirement.
	relayPath := filepath.Join(config, "native-relay.json")
	if _, err = mailmsg.SaveDomainRelay(ctx, relayPath, relayKey, mailmsg.DomainRelay{Domains: []string{"second.test"}, Issuer: nativeIssuer, Host: "smtp.example.test", Port: 465, Username: "operator", Password: "test-only"}); err != nil {
		t.Fatal(err)
	}
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); !errors.Is(err, ErrNativeDomainInUse) {
		t.Fatal("retired a domain the relay still sends for", err)
	}
	if err = os.Remove(relayPath); err != nil {
		t.Fatal(err)
	}
	holding, err := ingress.Open(filepath.Join(root, "receiving"), ingress.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err = holding.SetRoute(ctx, ingress.Route{Address: "two@second.test", Issuer: nativeIssuer, Subject: "two", Mailbox: two.ID, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// An abandoned SMTP transaction (staged, never accepted) never blocks it.
	if err = holding.Bind(ctx, "maddy-local", "abandoned", "", "two@second.test"); err != nil {
		t.Fatal(err)
	}
	if held, err := nativeHeldRecipients(ctx, root); err != nil || len(held) != 0 {
		t.Fatal("abandoned staged transaction counted as held mail", held, err)
	}
	if err = holding.Bind(ctx, "maddy-local", "held", "", "two@second.test"); err != nil {
		t.Fatal(err)
	}
	if err = holding.Accept(ctx, "maddy-local", "held", "", strings.NewReader("From: a@b.test\r\nTo: two@second.test\r\nSubject: held\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = holding.Close()
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); !errors.Is(err, ErrNativeDomainInUse) {
		t.Fatal("retired a domain with held incoming mail", err)
	}
	if err = os.RemoveAll(filepath.Join(root, "receiving")); err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(root, NativeRestoreHoldFile)
	if err = os.WriteFile(hold, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = domains.RetireDomain(ctx, "second.test", root, relayKey); err == nil {
		t.Fatal("retired a domain under a restore hold")
	}
	if err = os.Remove(hold); err != nil {
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
	if _, err = mailmsg.SetDomainRelayDomains(ctx, filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath(), []string{"other.test"}); err != nil {
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
	// With DNS gone, the deactivated owner's job still ends instead of
	// pinning the domain forever.
	sender.Domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
	if err = sender.Recover(ctx, u.ID, id); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("deactivated owner's job not ended without DNS", err)
	}
	if statuses, _, err := sender.Status(ctx, u.ID, id); err != nil || statuses[0].State != "quarantined" {
		t.Fatal("job not quarantined", statuses, err)
	}
	if err = sender.Domains.RetireDomain(ctx, "example.test", sender.StateRoot, sender.keyPath()); err != nil {
		t.Fatal(err)
	}
	// With no domain left the single-domain API reads as unconfigured.
	if d, err := sender.Domains.Read(); err != nil || d.Domain != "" {
		t.Fatal("founding after retirement", d, err)
	}
}

func TestNativeOutboundRecoverEndsOrRetriesByDomain(t *testing.T) {
	for _, mode := range []string{"retired", "relay-removed", "lapsed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			sender, u, job := outboundFixture(t)
			id, err := fsutil.NewUUIDv4()
			if err != nil {
				t.Fatal(err)
			}
			if err = sender.Queue(ctx, u.ID, id, job); err != nil {
				t.Fatal(err)
			}
			want, wantErr := "quarantined", ErrNativeOutboundStale
			switch mode {
			case "retired": // as if retired after the job turned retryable
				set, err := sender.Domains.ReadSet()
				if err != nil {
					t.Fatal(err)
				}
				delete(set.Domains, "example.test")
				set.Retired = append(set.Retired, "example.test")
				if err = sender.Domains.persistSet(set); err != nil {
					t.Fatal(err)
				}
			case "relay-removed":
				if _, err = mailmsg.SetDomainRelayDomains(ctx, filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath(), []string{"other.test"}); err != nil {
					t.Fatal(err)
				}
			case "lapsed":
				sender.Domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, nil })
				want, wantErr = "queued", ErrNativeDomain
			}
			if err = sender.Recover(ctx, u.ID, id); !errors.Is(err, wantErr) {
				t.Fatal("recovery outcome", err)
			}
			if statuses, _, err := sender.Status(ctx, u.ID, id); err != nil || statuses[0].State != want {
				t.Fatal("state", statuses, err)
			}
		})
	}
}

func TestNativeDisableOnlyRefusesReactivation(t *testing.T) {
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, "one", domains, accounts, nativeLimits)
	if err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 2, false)
	nativeDesired(t, life, "one", "one@example.test", 3, true)
	if _, err = life.DisableNativeMailboxContext(ctx, root, nativeIssuer, "one", u.ID, "example.test", nativeLimits); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("disable-only path reactivated", err)
	}
	if a, _, _ := life.NativeAssignment(nativeIssuer, "one"); a.Revision != 1 || !a.DesiredActive {
		t.Fatal("refused disable changed the ledger", a)
	}
	nativeDesired(t, life, "one", "one@example.test", 4, false)
	if a, err := life.DisableNativeMailboxContext(ctx, root, nativeIssuer, "one", u.ID, "example.test", nativeLimits); err != nil || a.DesiredActive || a.Revision != 4 {
		t.Fatal("disable", a, err)
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
	if err = queue(); !errors.Is(err, ErrNativeOutboundStale) {
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
	if err = queue(); !errors.Is(err, ErrNativeOutboundStale) {
		t.Fatal("From on a removed relay domain queued", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = mailmsg.SetDomainRelayDomains(ctx, path, sender.keyPath(), []string{"example.test"}); !errors.Is(err, mailmsg.ErrDomainRelay) {
		t.Fatal("domain-only update created a relay", err)
	}
}
