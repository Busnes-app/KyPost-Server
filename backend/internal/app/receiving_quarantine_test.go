//go:build linux

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const quarantineRaw = "From: header-from@outside.test\r\nSubject: secret-subject\r\n\r\nsecret-body\r\n"

// quarantineVia accepts one message to address, applies change, and imports
// it so it quarantines.
func quarantineVia(t *testing.T, r *receivingRuntime, id, address string, change func()) {
	t.Helper()
	ctx := context.Background()
	if err := r.bind(ctx, id, "envelope@outside.test", address); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(ctx, id, "envelope@outside.test", strings.NewReader(quarantineRaw)); err != nil {
		t.Fatal(err)
	}
	change()
	if err := r.importDelivery(ctx, id); !errors.Is(err, ingress.ErrRoute) {
		t.Fatal("not fenced", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, id); err != nil || d.State != "quarantined" {
		t.Fatal("not quarantined", d.State, err)
	}
}

func quarantineCLI(t *testing.T, r *receivingRuntime) func(args ...string) (string, error) {
	t.Setenv("CONFIG_DIR", r.configDir)
	t.Setenv("STATE_DIR", r.stateDir)
	t.Setenv("KYPOST_NATIVE_MAIL", "true")
	t.Setenv("KYPOST_NATIVE_RECEIVING", "true")
	return func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runReceivingQuarantine(args, &out)
		return out.String(), err
	}
}

// An alias reassigned to another user: release goes to the mailbox the mail
// was frozen to, once, and the CLI needs the ID confirmed.
func TestNativeQuarantineReleaseToFrozenMailbox(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	one, two := created[0], created[1]
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	quarantineVia(t, r, "frozen-at-one", "sales@example.test", func() {
		if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "sales@example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.life.ReassignNativeAddress(ctx, r.stateDir, "sales@example.test", two.ID); err != nil {
			t.Fatal(err)
		}
	})
	cli := quarantineCLI(t, r)
	listing, err := cli("list")
	if err != nil || !strings.Contains(listing, `"id":"frozen-at-one"`) || !strings.Contains(listing, `"sender":"envelope@outside.test"`) || !strings.Contains(listing, `"user":"`+one.ID+`"`) || !strings.Contains(listing, `"size":`) {
		t.Fatal("list", listing, err)
	}
	for _, secret := range []string{"secret-subject", "secret-body", "header-from"} {
		if strings.Contains(listing, secret) {
			t.Fatal("listing leaked message content", secret)
		}
	}
	for _, args := range [][]string{
		{"release", receivingGateway, "frozen-at-one"},
		{"release", receivingGateway, "frozen-at-one", "--confirm", "other"},
		{"release", receivingGateway, "frozen-at-one", "--yes", "frozen-at-one"},
	} {
		if _, err := cli(args...); err == nil {
			t.Fatal("unconfirmed release", args)
		}
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "frozen-at-one"); err != nil || d.State != "quarantined" {
		t.Fatal("refused release changed state", d.State, err)
	}
	// Local deactivation records nothing durable: not admitted, not retired.
	if _, err := r.accounts.Deactivate(one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cli("release", receivingGateway, "frozen-at-one", "--confirm", "frozen-at-one"); !errors.Is(err, sso.ErrQuarantineUnadmitted) {
		t.Fatal("transient refusal", err)
	}
	if _, err := r.accounts.Reactivate(one.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := cli("release", receivingGateway, "frozen-at-one", "--confirm", "frozen-at-one"); err != nil {
			t.Fatal(err)
		}
	}
	if got := receivingInbox(t, r, one); len(got) != 1 || got[0] != "secret-body" {
		t.Fatal("frozen owner", got)
	}
	if got := receivingInbox(t, r, two); len(got) != 0 {
		t.Fatal("current address owner received quarantined mail", got)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "frozen-at-one"); err != nil || d.Disposition != "released" {
		t.Fatal(d, err)
	}
	if _, err := cli("discard", receivingGateway, "frozen-at-one", "--confirm", "frozen-at-one"); !errors.Is(err, ingress.ErrNotQuarantined) {
		t.Fatal("released delivery discarded", err)
	}
}

// A disabled mailbox, a different frozen owner and an offboarded owner refuse
// release with the stated reason; discard still works and leaves a tombstone.
func TestNativeQuarantineReleaseRefusesChangedMailbox(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	m, err := r.life.CreateNativeMailbox(ctx, r.stateDir, created[0].ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	// Each is quarantined by an address change first, then its mailbox or
	// owner is made durably inactive while it waits.
	if _, err := r.life.AddNativeAlias(ctx, r.stateDir, m.ID, "alias@example.test"); err != nil {
		t.Fatal(err)
	}
	quarantineVia(t, r, "disabled", "alias@example.test", func() {
		if _, err := r.life.ReleaseNativeAlias(ctx, r.stateDir, "alias@example.test"); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := r.life.SetNativeMailboxState(ctx, r.stateDir, m.ID, false); err != nil {
		t.Fatal(err)
	}
	quarantineVia(t, r, "impostor", "two@example.test", func() {
		receivingDirectory(t, r, "two", 2, false)
		receivingDirectory(t, r, "two", 3, true)
	})
	// Model a frozen owner that no longer matches the mailbox's ledger owner.
	path, err := filepath.Abs(filepath.Join(r.stateDir, "receiving", "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE bindings SET subject='impostor' WHERE id='impostor'"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	quarantineVia(t, r, "offboarded", "one@example.test", func() {
		receivingDirectory(t, r, "one", 2, false)
		receivingDirectory(t, r, "one", 3, true)
	})
	receivingDirectory(t, r, "one", 4, false)
	cli := quarantineCLI(t, r)
	for _, id := range []string{"disabled", "impostor", "offboarded"} {
		if _, err := cli("release", receivingGateway, id, "--confirm", id); !errors.Is(err, sso.ErrQuarantineInactive) {
			t.Fatal(id, "release not refused with reason", err)
		}
		if d, err := r.holding.Get(ctx, receivingGateway, id); err != nil || d.State != "quarantined" {
			t.Fatal(id, "refused release changed state", d.State, err)
		}
	}
	if got := receivingInbox(t, r, created[1]); len(got) != 0 {
		t.Fatal("impostor-frozen mail reached the mailbox", got)
	}
	if out, err := cli("discard", receivingGateway, "disabled", "--confirm", "disabled"); err != nil || out != "discarded\n" {
		t.Fatal(out, err)
	}
	d, err := r.holding.Get(ctx, receivingGateway, "disabled")
	if err != nil || d.Disposition != "discarded" || len(d.Raw) != 0 {
		t.Fatal("discard", d, err)
	}
	// A re-pickup of the same receipt replays without resurrecting it.
	if err := r.holding.Accept(ctx, receivingGateway, "disabled", "envelope@outside.test", strings.NewReader(quarantineRaw)); err != nil {
		t.Fatal(err)
	}
	if err := r.importDelivery(ctx, "disabled"); err != nil {
		t.Fatal(err)
	}
	if listing, err := cli("list"); err != nil || strings.Contains(listing, `"disabled"`) || !strings.Contains(listing, `"impostor"`) {
		t.Fatal("list after discard", listing, err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, sso.NativeRestoreHoldFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.life.ReleaseQuarantined(ctx, r.stateDir, "https://identity.example.test", r.accounts, r.holding, receivingGateway, "impostor", false); !errors.Is(err, sso.ErrNativeRestoreHold) {
		t.Fatal("release under restore hold", err)
	}
	if _, err := cli("discard", receivingGateway, "impostor", "--confirm", "impostor"); !errors.Is(err, sso.ErrNativeRestoreHold) {
		t.Fatal("discard under restore hold", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "impostor"); err != nil || d.State != "quarantined" {
		t.Fatal("restore hold lost quarantined mail", d.State, err)
	}
}

// Pending mail whose frozen owner is durably inactive quarantines instead of
// holding receiving capacity forever; local deactivation stays pending.
func TestNativeReceivingQuarantinesDurablyInactiveOwners(t *testing.T) {
	ctx := context.Background()
	t.Run("disabled mailbox, then released after re-enable", func(t *testing.T) {
		r, created := receivingFixture(t)
		m, err := r.life.CreateNativeMailbox(ctx, r.stateDir, created[0].ID, "sales@example.test")
		if err != nil {
			t.Fatal(err)
		}
		quarantineVia(t, r, "disabled", "sales@example.test", func() {
			if _, err := r.life.SetNativeMailboxState(ctx, r.stateDir, m.ID, false); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := r.life.SetNativeMailboxState(ctx, r.stateDir, m.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := r.life.ReleaseQuarantined(ctx, r.stateDir, "https://identity.example.test", r.accounts, r.holding, receivingGateway, "disabled", false); err != nil {
			t.Fatal(err)
		}
		if got := receivingMailbox(t, r, m.ID); len(got) != 1 || got[0] != "secret-body" {
			t.Fatal("re-enabled mailbox", got)
		}
	})
	t.Run("restore hold leaves even a disabled mailbox pending", func(t *testing.T) {
		r, created := receivingFixture(t)
		m, err := r.life.CreateNativeMailbox(ctx, r.stateDir, created[0].ID, "sales@example.test")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.bind(ctx, "held", "envelope@outside.test", "sales@example.test"); err != nil {
			t.Fatal(err)
		}
		if err := r.accept(ctx, "held", "envelope@outside.test", strings.NewReader(quarantineRaw)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.life.SetNativeMailboxState(ctx, r.stateDir, m.ID, false); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.stateDir, sso.NativeRestoreHoldFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.importDelivery(ctx, "held"); !errors.Is(err, sso.ErrNativeRestoreHold) {
			t.Fatal(err)
		}
		if d, err := r.holding.Get(ctx, receivingGateway, "held"); err != nil || d.State != "pending" {
			t.Fatal("restore hold quarantined", d.State, err)
		}
	})
	t.Run("directory offboarding", func(t *testing.T) {
		r, _ := receivingFixture(t)
		quarantineVia(t, r, "offboarded", "one@example.test", func() { receivingDirectory(t, r, "one", 2, false) })
	})
	t.Run("promotion to administrator", func(t *testing.T) {
		r, created := receivingFixture(t)
		quarantineVia(t, r, "promoted", "one@example.test", func() {
			raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":true,"roles":["kypost.admin"],"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`)
			var resource sso.DirectoryUser
			if err := json.Unmarshal(raw, &resource); err != nil {
				t.Fatal(err)
			}
			ev := syncauth.Event{ID: "promote-one", Type: "user.updated", At: time.Now()}
			if _, err := r.life.ApplyDirectoryUser("https://identity.example.test", ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return true, nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := r.accounts.SetRole(created[0].ID, users.RoleAdmin); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("local deactivation stays pending", func(t *testing.T) {
		r, created := receivingFixture(t)
		if err := r.bind(ctx, "local", "envelope@outside.test", "one@example.test"); err != nil {
			t.Fatal(err)
		}
		if err := r.accept(ctx, "local", "envelope@outside.test", strings.NewReader(quarantineRaw)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.accounts.Deactivate(created[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := r.importDelivery(ctx, "local"); err == nil || errors.Is(err, ingress.ErrRoute) {
			t.Fatal("transient refusal", err)
		}
		if d, err := r.holding.Get(ctx, receivingGateway, "local"); err != nil || d.State != "pending" {
			t.Fatal("transient refusal quarantined", d.State, err)
		}
	})
}

func TestNativeQuarantineCLIRefusesOtherUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every candidate directory")
	}
	r, _ := receivingFixture(t)
	cli := quarantineCLI(t, r)
	t.Setenv("STATE_DIR", "/")
	if _, err := cli("list"); err == nil || !strings.Contains(err.Error(), "docker compose exec --user kypost kypost-server kypost-server receiving quarantine") {
		t.Fatal("ran as another user than the state owner", err)
	}
}
