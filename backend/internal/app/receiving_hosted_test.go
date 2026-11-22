//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// RX PR 5: the hosted profile's bind. K's org check (exit 12, or the unknown-address 3)
// and the relay domain's refused-recipient budget (exit 13). Multi mode cannot start
// until K PR 7, so these build the runtime directly, as K's tests do.

const probeSource = "192.0.2.7"

func setRelay(t *testing.T, r *receivingRuntime, domains string) {
	t.Helper()
	if err := r.state.SetSetting(state.MaskedDomainsSetting, domains); err != nil {
		t.Fatal(err)
	}
}

// bindExit is the helper's exit code for one hosted bind.
func bindExit(r *receivingRuntime, id, recipient, source string) int {
	err := r.bindFrom(context.Background(), id, tlsSender, recipient, source)
	if err == nil {
		return 0
	}
	return receivingExit(err)
}

// A suspended, disabled or uncached org answers 451 4.2.1 (exit 12); deletion_scheduled
// and deleted answer exactly as an unknown address (exit 3, 550 5.1.1); the staff org
// always receives.
func TestHostedBindOrgStatus(t *testing.T) {
	r, _ := importFixture(t, receivingOrgA, receivingOrgB)
	ctx := context.Background()
	unknown := r.bind(ctx, "unknown", tlsSender, "nobody@example.test")
	if receivingExit(unknown) != 3 {
		t.Fatalf("unknown address: %v", unknown)
	}
	status := func(org, s string, enabled bool) {
		t.Helper()
		o := suitedir.Org{ID: org, Status: s, Revision: orgRevision.Add(1), Enabled: enabled, Quotas: json.RawMessage(`{}`)}
		if err := tenancy.ApplyOrg(ctx, r.tenancy, o, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	want := func(label string, code int) {
		t.Helper()
		err := r.bind(ctx, label, tlsSender, "one@example.test")
		got := 0
		if err != nil {
			got = receivingExit(err)
		}
		if got != code {
			t.Fatalf("%s: %v, want exit %d", label, err, code)
		}
		if code == 3 && err.Error() != unknown.Error() {
			t.Fatalf("%s answers unlike an unknown address: %v", label, err)
		}
		if code != 0 {
			if s := stateOf(t, r, receivingGateway, label); s != "gone" {
				t.Fatalf("%s left a delivery: %s", label, s)
			}
		}
	}
	want("active", 0)
	status(receivingOrgA, suitedir.OrgSuspended, true)
	want("suspended", 12)
	status(receivingOrgA, suitedir.OrgActive, false)
	want("disabled", 12)
	if err := tenancy.ForgetRestored(ctx, r.tenancy); err != nil {
		t.Fatal(err)
	}
	want("uncached", 12)
	status(receivingOrgA, suitedir.OrgDeletionScheduled, true)
	want("scheduled", 3)
	status(receivingOrgA, suitedir.OrgDeleted, true)
	want("deleted", 3)

	// The staff org is never gated (K8), whatever the cache says.
	t.Setenv("KYPOST_TENANCY", config.TenancyMulti)
	t.Setenv("KYPOST_STAFF_ORG_ID", receivingOrgB)
	status(receivingOrgB, suitedir.OrgSuspended, false)
	if err := r.bind(ctx, "staff", tlsSender, "two@example.test"); err != nil {
		t.Fatalf("staff org mailbox: %v", err)
	}
}

// Past 50 relay-domain refusals in 10 minutes from one source, every relay-domain RCPT
// from it answers 450 4.7.1 (exit 13), live, masked and unknown alike, before any other
// check. IPv4 counts per /32 and IPv6 per /64; other sources are untouched.
func TestHostedBindProbeBudget(t *testing.T) {
	r, created := importFixture(t, receivingOrgA, receivingOrgB)
	ctx := context.Background()
	setRelay(t, r, "example.test")
	const masked = "quiet.otter.k7q2xm@example.test"
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, masked); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) {
		s.MaskedAddresses = []config.MaskedAddress{{Address: masked, State: "active", CreatedAt: now, ExpiresAt: now + 3600}}
	})
	// M: a paused masked address answers as an unknown one, and is counted like one.
	paused := func() {
		setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) {
			s.MaskedAddresses = []config.MaskedAddress{{Address: masked, State: "paused", CreatedAt: now}}
		})
	}
	probe := func(source string, n int) {
		t.Helper()
		for i := range n {
			if code := bindExit(r, fmt.Sprintf("p-%s-%d", source, i), fmt.Sprintf("guess%d@example.test", i), source); code != 3 {
				t.Fatalf("refusal %d from %s: exit %d", i+1, source, code)
			}
		}
	}
	// Only unknown-recipient refusals count: a sender refusal (exit 7) is not a probe.
	for i := range 2 * probeLimit {
		if err := r.bindFrom(ctx, fmt.Sprint("s", i), "not a sender", "one@example.test", "192.0.2.9"); receivingExit(err) != 7 {
			t.Fatalf("sender refusal: %v", err)
		}
	}
	if code := bindExit(r, "after-sender-refusals", "one@example.test", "192.0.2.9"); code != 0 {
		t.Fatalf("sender refusals were counted: exit %d", code)
	}
	if code := bindExit(r, "live-first", masked, probeSource); code != 0 {
		t.Fatalf("live masked address: exit %d", code)
	}
	probe(probeSource, probeLimit-1)
	paused()
	if code := bindExit(r, "paused", masked, probeSource); code != 3 {
		t.Fatalf("paused masked address: exit %d", code)
	}
	// The 50th refusal started the block.
	for _, rcpt := range []string{"one@example.test", "one+tag@example.test", masked, "never-existed@example.test", "two@example.test"} {
		if code := bindExit(r, "blocked-"+rcpt, rcpt, probeSource); code != 13 {
			t.Fatalf("%s past the budget: exit %d, want 13", rcpt, code)
		}
	}
	if code := bindExit(r, "mapped", "one@example.test", "::ffff:"+probeSource); code != 13 {
		t.Fatalf("IPv4-mapped source: exit %d", code)
	}
	if code := bindExit(r, "neighbour", "one@example.test", "192.0.2.8"); code != 0 {
		t.Fatalf("another /32: exit %d", code)
	}

	probe("2001:db8:1:2::1", probeLimit)
	if code := bindExit(r, "same-64", "one@example.test", "2001:db8:1:2:ffff::9"); code != 13 {
		t.Fatalf("same /64: exit %d", code)
	}
	if code := bindExit(r, "other-64", "one@example.test", "2001:db8:1:3::1"); code != 0 {
		t.Fatalf("another /64: exit %d", code)
	}

	// No peer IP on a relay-domain recipient: temporary, never unbudgeted.
	if code := bindExit(r, "no-source", "one@example.test", ""); code != 1 {
		t.Fatalf("relay recipient without a source: exit %d", code)
	}
	if got := inbox(t, r, created[0]); len(got) != 0 {
		t.Fatalf("mail imported during probing: %v", got)
	}
}

// The block lasts probeBlock, then the source is answered normally again.
func TestHostedBindProbeBlockEnds(t *testing.T) {
	r, _ := importFixture(t, receivingOrgA, receivingOrgB)
	setRelay(t, r, "example.test")
	defer func(b time.Duration) { probeBlock = b }(probeBlock)
	probeBlock = 2 * time.Second
	for i := range probeLimit {
		_ = bindExit(r, fmt.Sprint("p", i), fmt.Sprintf("guess%d@example.test", i), probeSource)
	}
	if code := bindExit(r, "blocked", "one@example.test", probeSource); code != 13 {
		t.Fatalf("blocked: exit %d", code)
	}
	time.Sleep(probeBlock + 500*time.Millisecond)
	if code := bindExit(r, "after", "one@example.test", probeSource); code != 0 {
		t.Fatalf("after the block: exit %d", code)
	}
}

// Primary domains are never budgeted: no window is written, and the hundredth refusal
// is still 550.
func TestHostedBindPrimaryDomainsNeverBudgeted(t *testing.T) {
	for name, relay := range map[string]string{"another relay domain": "relay.example.test", "no masked domains": ""} {
		t.Run(name, func(t *testing.T) {
			r, _ := importFixture(t, receivingOrgA, receivingOrgB)
			if relay != "" {
				setRelay(t, r, relay)
			}
			for i := range 2 * probeLimit {
				if code := bindExit(r, fmt.Sprint("p", i), fmt.Sprintf("guess%d@example.test", i), probeSource); code != 3 {
					t.Fatalf("refusal %d: exit %d", i+1, code)
				}
			}
			if code := bindExit(r, "live", "one@example.test", probeSource); code != 0 {
				t.Fatalf("live primary: exit %d", code)
			}
			for _, key := range []string{"receiving-probe:" + probeSource + "/32", "receiving-probe-block:" + probeSource + "/32"} {
				if hits, _, err := r.state.AttemptHits(context.Background(), key); err != nil || hits != 0 {
					t.Fatalf("%s: %d %v", key, hits, err)
				}
			}
		})
	}
}

// TestCrossOrgMatrix's SMTP RCPT row (K "Proving a cross-org ID"), over the hosted profile:
// an MX host's bind and accept with the peer IP, through TLS, deliver to B's address into B
// and never A; probing the relay domain over the same path ends in 450 for live and
// unknown addresses alike.
func TestCrossOrgMatrixSMTPRcpt(t *testing.T) {
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
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{addr})
	if err := runReceivingCommand([]string{"bind", "to-b", tlsSender, "two@example.test", probeSource}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runReceivingCommand([]string{"accept", "to-b", tlsSender, probeSource, "mx.outside.test"}, bytes.NewReader(tlsMessage)); err != nil {
		t.Fatal(err)
	}
	pass(t, f.r, "w")
	one, two := users.User{NativeMailboxIssuer: issuer, SSOSub: "one"}, users.User{NativeMailboxIssuer: issuer, SSOSub: "two"}
	if got := inbox(t, f.r, two); len(got) != 1 {
		t.Fatalf("B's mailbox: %v", got)
	}
	if got := inbox(t, f.r, one); len(got) != 0 {
		t.Fatalf("A's mailbox received B's mail: %v", got)
	}

	setRelay(t, f.r, "example.test")
	for i := range probeLimit {
		if code := exitCode(runReceivingCommand([]string{"bind", fmt.Sprint("p", i), tlsSender, fmt.Sprintf("guess%d@example.test", i), probeSource}, nil)); code != 3 {
			t.Fatalf("refusal %d over TLS: exit %d", i+1, code)
		}
	}
	for _, rcpt := range []string{"one@example.test", "guess-next@example.test"} {
		if code := exitCode(runReceivingCommand([]string{"bind", "past-" + rcpt, tlsSender, rcpt, probeSource}, nil)); code != 13 {
			t.Fatalf("%s past the budget over TLS: exit %d", rcpt, code)
		}
	}
}

// The source argument exists only in multi mode, and only as one literal IP.
func TestHostedBindArguments(t *testing.T) {
	bind := []string{"bind", "x", tlsSender, "one@example.test", probeSource}
	if err := checkReceivingArgs(bind); err == nil {
		t.Fatal("single-tenant bind took a source argument")
	}
	t.Setenv("KYPOST_TENANCY", config.TenancyMulti)
	t.Setenv("KYPOST_STAFF_ORG_ID", tlsStaffOrg)
	if err := checkReceivingArgs(bind); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"", "192.0.2.7/32", "fe80::1%eth0", "mx.outside.test", "192.0.2.7 ", "\x00"} {
		if err := checkReceivingArgs(append(slices.Clone(bind[:4]), source)); err == nil {
			t.Fatalf("source %q accepted", source)
		}
	}
	if err := checkReceivingArgs(append(slices.Clone(bind), "extra")); err == nil {
		t.Fatal("six bind arguments accepted")
	}
	var commandError *receivingCommandError
	if !errors.As(errOrgUnavailable, &commandError) || commandError.code != 12 || !errors.As(errProbeBudget, &commandError) || commandError.code != 13 {
		t.Fatal("hosted exit codes changed")
	}
}
