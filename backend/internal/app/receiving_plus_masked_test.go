//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func setAddressSettings(t *testing.T, r *receivingRuntime, userID string, mutate func(*config.UserSettings)) {
	t.Helper()
	if err := config.UpdateUserSettings(config.UserSettingsPath(r.configDir, userID), func(s *config.UserSettings) error {
		mutate(s)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Plus addressing: user+tag@ lands in user's mailbox once, the tag never
// selects another address, and a user who turned it off gets the unknown-
// address refusal.
func TestNativeReceivingPlusAddressing(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[1].ID, "one+two@example.test"); !errors.Is(err, sso.ErrNativeAddressInvalid) {
		t.Fatal("an alias holding + was issued", err)
	}
	receive(t, r, "tagged", "One+Receipts@example.test", "one@example.test", "sales+news@example.test")
	if err := r.importDelivery(ctx, "tagged"); err != nil {
		t.Fatal(err)
	}
	if got := receivingInbox(t, r, created[0]); strings.Join(got, ",") != "tagged" {
		t.Fatal("plus mail not delivered once to the base address's mailbox", got)
	}
	unknown := r.bind(ctx, "unknown", "", "nobody+x@example.test")
	if !errors.Is(unknown, ingress.ErrRoute) {
		t.Fatal("tag on an unknown address routed", unknown)
	}
	setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) { s.RefusePlusAddresses = true })
	if err := r.bind(ctx, "refused", "", "one+x@example.test"); !errors.Is(err, ingress.ErrRoute) || receivingExit(err) != receivingExit(unknown) {
		t.Fatal("refused plus address answered unlike an unknown address", err)
	}
	if err := r.bind(ctx, "plain", "", "one@example.test"); err != nil {
		t.Fatal("plus refusal stopped plain mail", err)
	}
	if err := r.bind(ctx, "other", "", "two+x@example.test"); err != nil {
		t.Fatal("one user's plus refusal reached another", err)
	}
}

// A paused, expired or deleted masked address answers exactly as an unknown
// address does; a live one delivers.
func TestNativeReceivingMaskedStates(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const masked = "quiet.otter.4821@example.test"
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, created[0].ID, masked); err != nil {
		t.Fatal(err)
	}
	unknown := r.bind(ctx, "unknown", "", "nobody@example.test")
	refusedLikeUnknown := func(label string) {
		t.Helper()
		err := r.bind(ctx, label, "", masked)
		if !errors.Is(err, ingress.ErrRoute) || err.Error() != unknown.Error() || receivingExit(err) != receivingExit(unknown) {
			t.Fatal(label, "masked refusal differs from an unknown address", err, unknown)
		}
	}
	now := time.Now().Unix()
	for _, m := range []config.MaskedAddress{
		{Address: masked, State: "paused", CreatedAt: now},
		{Address: masked, State: "deleted", CreatedAt: now},
		{Address: masked, State: "active", CreatedAt: now, ExpiresAt: 1},
	} {
		setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) { s.MaskedAddresses = []config.MaskedAddress{m} })
		refusedLikeUnknown(m.State)
	}
	setAddressSettings(t, r, created[0].ID, func(s *config.UserSettings) {
		s.MaskedAddresses = []config.MaskedAddress{{Address: masked, State: "active", CreatedAt: now, ExpiresAt: now + 3600}}
	})
	receive(t, r, "live", masked)
	if err := r.importDelivery(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if got := receivingInbox(t, r, created[0]); strings.Join(got, ",") != "live" {
		t.Fatal("live masked address did not deliver", got)
	}
	// The ledger half: a released (paused or deleted) address refuses even
	// when the record says active.
	if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, masked); err != nil {
		t.Fatal(err)
	}
	refusedLikeUnknown("released")
}

// The Cloudflare table carries noPlus for a user who refuses plus addresses
// and leaves out a masked address that is not live, with no stale fallback.
func TestCloudflareRoutesPlusAndMasked(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	const masked = "quiet.otter.4821@example.test"
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, e.users[0].ID, masked); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	routes, err := e.l.buildRoutes(ctx)
	if err != nil || len(routes) != 3 || routes[1].Address != masked || routes[1].NoPlus {
		t.Fatalf("routes before %+v, %v", routes, err)
	}
	now := time.Now().Unix()
	setAddressSettings(t, e.r, e.users[0].ID, func(s *config.UserSettings) {
		s.RefusePlusAddresses = true
		s.MaskedAddresses = []config.MaskedAddress{{Address: masked, State: "active", CreatedAt: now, ExpiresAt: 1}}
	})
	if routes, err = e.l.buildRoutes(ctx); err != nil || len(routes) != 2 {
		t.Fatalf("expired masked address still routed: %+v, %v", routes, err)
	}
	for _, route := range routes {
		if route.NoPlus != (route.Mailbox == e.users[0].ID) {
			t.Fatalf("noPlus on the wrong routes: %+v", routes)
		}
	}
}

// Addresses holding a + from before plus addressing keep receiving: a full
// active address wins over its base. A KyIdentity primary holding + still
// provisions; a tag on an address with no exact match reaches the base.
func TestNativeReceivingKeepsExistingPlusAddresses(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	const issuer = "https://identity.example.test"
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"john","externalId":"john","userName":"john","active":true,"emails":[{"value":"john+work@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "create-john", Type: "user.created", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal("directory event with a + primary", err)
	}
	john, err := r.life.AllocateNativeAccount(ctx, r.stateDir, issuer, "john", r.domains, r.accounts, mailbox.Limits{MessageBytes: 5 << 20, PayloadBytes: 32 << 20, Records: 10000})
	if err != nil {
		t.Fatal("provisioning a + primary", err)
	}
	legacyPlusAlias(t, r.configDir, "one+legacy@example.test", created[0].ID)
	// two+x has no exact record: the base two@ receives it.
	receive(t, r, "john-primary", "john+work@example.test")
	receive(t, r, "legacy-alias", "One+Legacy@example.test")
	receive(t, r, "base", "two+x@example.test")
	for _, id := range []string{"john-primary", "legacy-alias", "base"} {
		if err := r.importDelivery(ctx, id); err != nil {
			t.Fatal(id, err)
		}
	}
	for u, want := range map[string]string{john.ID: "john-primary", created[0].ID: "legacy-alias", created[1].ID: "base"} {
		var user users.User
		for _, c := range append(created, john) {
			if c.ID == u {
				user = c
			}
		}
		if got := receivingInbox(t, r, user); strings.Join(got, ",") != want {
			t.Fatal("delivered", got, "want", want)
		}
	}
	// A tag on the legacy alias resolves to its base (one@), not the alias.
	receive(t, r, "tag-on-legacy", "one+legacy+x@example.test")
	if err := r.importDelivery(ctx, "tag-on-legacy"); err != nil {
		t.Fatal(err)
	}
	if got := receivingInbox(t, r, created[0]); len(got) != 2 {
		t.Fatal("tagged legacy alias", got)
	}
}

// legacyPlusAlias writes a + alias as an earlier release could issue it.
func legacyPlusAlias(t *testing.T, configDir, address, mailboxID string) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO addresses(address, mailbox, kind, state, generation) VALUES ('" + address + "', '" + mailboxID + "', 'alias', 'active', 1)",
		"INSERT INTO address_history(address, generation, mailbox) VALUES ('" + address + "', 1, '" + mailboxID + "')",
	} {
		if err := ssotest.DamageAuthority(configDir, q); err != nil {
			t.Fatal(err)
		}
	}
}

// two+legacy@ belongs to one, two@ to two. Once two+legacy@ is reserved its
// mail is refused, never handed to two, and the Worker table refuses it.
func TestInactiveLegacyPlusAddressKeepsItsOwner(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	legacyPlusAlias(t, r.configDir, "two+legacy@example.test", created[0].ID)
	receive(t, r, "live", "two+legacy@example.test")
	if err := r.importDelivery(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if got := receivingInbox(t, r, created[0]); strings.Join(got, ",") != "live" {
		t.Fatal("active legacy address", got)
	}
	if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "two+legacy@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.bind(ctx, "after", "", "two+legacy@example.test"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("reserved legacy address fell back to its base", err)
	}
	if got := receivingInbox(t, r, created[1]); len(got) != 0 {
		t.Fatal("base owner received", got)
	}

	e := cfFixture(t)
	legacyPlusAlias(t, e.r.configDir, "two+legacy@example.test", e.users[0].ID)
	if _, err := e.r.life.ReleaseNativeAlias(ctx, e.r.stateDir, "two+legacy@example.test"); err != nil {
		t.Fatal(err)
	}
	routes, err := e.l.buildRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refused := false
	for _, route := range routes {
		refused = refused || route.Address == "two+legacy@example.test" && route.Refused
	}
	if !refused {
		t.Fatalf("no refusal published for the reserved legacy address: %+v", routes)
	}
}
