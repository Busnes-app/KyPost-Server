//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeReceivingSeveralDomains(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const issuer = "https://identity.example.test"
	if _, err := r.domains.ConfigureDomain(ctx, "second.test", issuer); err != nil {
		t.Fatal(err)
	}
	lapsed := map[string]bool{}
	r.domains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		set, err := r.domains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." && !lapsed[d.Domain] {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, err
	})
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"three","externalId":"three","userName":"three","active":true,"emails":[{"value":"three@second.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "create-three", Type: "user.created", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	three, err := r.life.AllocateNativeAccount(ctx, r.stateDir, issuer, "three", r.domains, r.accounts, mailbox.Limits{MessageBytes: 5 << 20, PayloadBytes: 32 << 20, Records: 10000})
	if err != nil {
		t.Fatal(err)
	}
	// One delivery to both domains reaches both mailboxes.
	for _, address := range []string{"one@example.test", "three@second.test"} {
		if err := r.bind(ctx, "both", "sender@outside.test", address); err != nil {
			t.Fatal(address, err)
		}
	}
	message := []byte("From: sender@outside.test\r\nSubject: two domains\r\n\r\nbody\r\n")
	if err := r.accept(ctx, "both", "sender@outside.test", bytes.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, "both"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{created[0].ID, three.ID} {
		u, err := r.accounts.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		a, _, err := r.life.NativeAssignment(issuer, u.SSOSub)
		if err != nil {
			t.Fatal(err)
		}
		box, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", id, "mailbox"), a.Owner, a.Limits, a.Source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := box.Raw(ctx, "INBOX", 1)
		_ = box.Close()
		if err != nil || !bytes.Equal(got, message) {
			t.Fatal("delivery missing on", a.Address, err)
		}
	}
	if err := r.bind(ctx, "unknown", "", "x@unknown.test"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("unconfigured domain routed", err)
	}
	// A lapsed second domain refuses its binds; the first keeps receiving.
	lapsed["second.test"] = true
	if err := r.bind(ctx, "lapsed", "", "three@second.test"); !errors.Is(err, sso.ErrNativeDomain) {
		t.Fatal("lapsed domain bound", err)
	}
	if err := r.bind(ctx, "first", "", "one@example.test"); err != nil {
		t.Fatal("lapse of one domain blocked another", err)
	}
	if err := r.accept(ctx, "first", "", bytes.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	// Re-verifying or re-challenging the second domain never fences the first.
	lapsed["second.test"] = false
	proofs, err := r.verifyDomains(ctx, []string{"one@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.domains.VerifyDomain(ctx, "second.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.domains.ConfigureDomain(ctx, "second.test", issuer); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = r.withAuthority(ctx, []string{created[0].ID}, []string{"one@example.test"}, proofs, func(map[string]sso.NativeAssignment) error { called = true; return nil }); err != nil || !called {
		t.Fatal("second domain fenced the first", err)
	}
}

func TestNativeReceivingConfigListsEveryDomain(t *testing.T) {
	r, _ := receivingFixture(t)
	ctx := context.Background()
	for _, domain := range []string{"second.test", "third.test"} {
		if _, err := r.domains.ConfigureDomain(ctx, domain, "https://identity.example.test"); err != nil {
			t.Fatal(err)
		}
	}
	// third.test never proves; it is still listed and its binds refuse.
	r.domains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		set, err := r.domains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." && d.Domain != "third.test" {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, err
	})
	if err := r.domains.RetireDomain(ctx, "third.test", r.stateDir, filepath.Join(t.TempDir(), "absent.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.domains.ConfigureDomain(ctx, "fourth.test", "https://identity.example.test"); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cert, key, leaf := receivingTestCertificate(t, t.TempDir())
	var output bytes.Buffer
	if err := r.writeConfig(ctx, executable, "127.0.0.1:2525", leaf.DNSNames[0], cert, key, "", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "    destination example.test fourth.test second.test { deliver_to dummy }\n") || strings.Contains(output.String(), "third.test") {
		t.Fatal("destinations", output.String())
	}
}
