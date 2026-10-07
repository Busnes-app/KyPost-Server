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
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Mail bound under v1 behaves identically after migration: a binding at an
// older directory revision still quarantines, one at the current revision
// still imports, and no address generation is below a route or binding.
// v1 routed at the directory revision, so the fixture writes those routes and
// accepts directly, as the v1 binary did.
func TestNativeMigrationKeepsReceivingBindings(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	u := created[0]
	accept := func(id, body string, revision int64) {
		t.Helper()
		route := ingress.Route{Address: "one@example.test", Issuer: u.NativeMailboxIssuer, Subject: u.SSOSub, Mailbox: u.ID, Generation: revision, Active: true, ValidUntil: time.Now().Add(time.Minute)}
		if err := r.holding.SetRoute(ctx, route); err != nil {
			t.Fatal(err)
		}
		if err := r.holding.Bind(ctx, receivingGateway, id, "", route.Address); err != nil {
			t.Fatal(err)
		}
		if err := r.holding.Accept(ctx, receivingGateway, id, "", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	accept("bound-at-revision-one", "From: a@outside.test\r\n\r\nold\r\n", 1)
	directory, _, err := r.life.Directory(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	resource := *directory.Resource
	resource.Meta.Version = `W/"2"`
	encoded, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	event := syncauth.Event{ID: "newer-same-owner", Type: "user.updated", At: time.Now()}
	if _, err := r.life.ApplyDirectoryUser(u.NativeMailboxIssuer, event, resource, sso.EventDigest(event.Type, encoded), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	accept("bound-at-revision-two", "From: b@outside.test\r\n\r\ncurrent\r\n", 2)
	keyPath := filepath.Join(t.TempDir(), "native-relay.key")
	if err := sso.WriteNativeV1ForTest(r.configDir, keyPath); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, "bound-at-revision-two"); err == nil {
		t.Fatal("unmigrated storage imported mail")
	}
	if migrated, err := sso.MigrateNative(ctx, r.configDir, keyPath); err != nil || !migrated {
		t.Fatal(migrated, err)
	}
	var ledger struct {
		Addresses map[string]struct{ Generation int64 } `json:"addresses"`
	}
	raw, err := os.ReadFile(filepath.Join(r.configDir, "native-provisioning.json"))
	if err != nil || json.Unmarshal(raw, &ledger) != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bound-at-revision-one", "bound-at-revision-two"} {
		d, err := r.holding.Get(ctx, receivingGateway, id)
		if err != nil {
			t.Fatal(err)
		}
		if g := ledger.Addresses[d.Bindings[0].Address].Generation; g < d.Bindings[0].Generation {
			t.Fatalf("address generation %d below binding generation %d", g, d.Bindings[0].Generation)
		}
	}
	if err := r.importDelivery(ctx, "bound-at-revision-one"); !errors.Is(err, ingress.ErrRoute) {
		t.Fatalf("older-revision binding was not fenced: %v", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "bound-at-revision-one"); err != nil || d.State != "quarantined" || !bytes.Contains(d.Raw, []byte("old")) {
		t.Fatalf("older-revision delivery not quarantined: %v", err)
	}
	if err := r.importDelivery(ctx, "bound-at-revision-two"); err != nil {
		t.Fatalf("current-revision binding did not import: %v", err)
	}
}

func TestMigrateNativeCommandReportsRemediation(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CONFIG_DIR", config)
	t.Setenv("SECRET_DIR", t.TempDir())
	if err := Run([]string{"migrate-native"}); err != nil {
		t.Fatal("fresh install", err)
	}
	if err := Run([]string{"migrate-native", "extra"}); err == nil {
		t.Fatal("extra argument accepted")
	}
	if err := os.WriteFile(filepath.Join(config, "native-domain.json"), []byte(`{"domain":"example.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"migrate-native"}); err == nil || !strings.Contains(err.Error(), "restore the pre-migration backup") {
		t.Fatal("failure without remediation", err)
	}
}
