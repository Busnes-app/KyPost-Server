//go:build linux

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
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
	if _, _, active := receivingRoute(t, r, "one@example.test"); !active {
		t.Fatal("tagged mail did not bind the base route")
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
