package sso

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const heldOrg = "1a2b3c4d-0000-4000-8000-00000000000a"

// A queued send of an org-bound owner is held, never claimed or quarantined, while the
// org is not served, and with no gate at all; it stays queued for the resume (tenancy
// design "Org status": queued sends are held, not sent).
func TestNativeOutboundHoldsANotServedOrg(t *testing.T) {
	ctx := context.Background()
	sender, _, _ := outboundFixture(t)
	hostedDomainsFixture(t, sender.ConfigDir)
	hostedDesired(t, NewLifecycleStore(sender.ConfigDir), "two", "two@example.test", "two@example.test", 1)
	u, err := NewLifecycleStore(sender.ConfigDir).AllocateNativeAccount(ctx, sender.StateRoot, nativeIssuer, "two", sender.Domains, sender.Accounts, nativeLimits, heldOrg)
	if err != nil || u.OrgID != heldOrg {
		t.Fatal(u.OrgID, err)
	}
	_, err = mailmsg.SaveDomainRelay(ctx, filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath(), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: nativeIssuer, Host: "127.0.0.1", Port: 1, Username: "operator", Password: "test-only"})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	apply := func(status string, rev int64) {
		if err := tenancy.ApplyOrg(ctx, cache, suitedir.Org{ID: heldOrg, Status: status, Revision: rev, Enabled: true, Quotas: json.RawMessage(`{}`)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	job := heldJob(u)
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, job); err != nil {
		t.Fatal(err)
	}
	queued := func(when string) {
		t.Helper()
		statuses, _, err := sender.Status(ctx, u.ID, id)
		if err != nil || len(statuses) != 1 || statuses[0].State != "queued" || statuses[0].Attempts != 0 {
			t.Fatalf("%s: the held job changed: %+v %v", when, statuses, err)
		}
	}
	var nilGate *tenancy.Gate
	for name, gate := range map[string]interface {
		Serve(context.Context, string) error
	}{
		"no gate":    nil,
		"nil gate":   nilGate,
		"no cache":   &tenancy.Gate{},
		"not cached": &tenancy.Gate{DB: cache},
	} {
		sender.Orgs = gate
		if _, err := sender.Submit(ctx, u.ID, id, 0); !errors.Is(err, ErrNativeOutboundHeld) {
			t.Fatalf("%s: Submit %v, want held", name, err)
		}
		queued(name)
	}
	sender.Orgs = &tenancy.Gate{DB: cache}
	for i, status := range []string{"suspended", "deletion_scheduled"} {
		apply(status, int64(i+1))
		if _, err := sender.Submit(ctx, u.ID, id, 0); !errors.Is(err, ErrNativeOutboundHeld) {
			t.Fatalf("%s: Submit %v, want held", status, err)
		}
		if err := sender.Recover(ctx, u.ID, id); err != nil {
			t.Fatalf("%s: Recover %v; a held job is not an error", status, err)
		}
		queued(status)
	}
	// Resumed: the job is claimed again (the closed port then refuses it).
	apply("active", 3)
	if _, err := sender.Submit(ctx, u.ID, id, 0); errors.Is(err, ErrNativeOutboundHeld) {
		t.Fatal("an active org's send is still held")
	}
	statuses, _, err := sender.Status(ctx, u.ID, id)
	if err != nil || len(statuses) != 1 || statuses[0].Attempts != 1 {
		t.Fatalf("resumed send was not attempted: %+v %v", statuses, err)
	}
}

func heldJob(u users.User) mailbox.OutboundJob {
	return mailbox.OutboundJob{From: "two@example.test", NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"recipient@example.test"}, Raw: []byte("From: two@example.test\r\nTo: recipient@example.test\r\nSubject: held\r\n\r\nmessage\r\n")}}}
}
