//go:build linux

package processor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativePollerStateAdmissionBeforeCache(t *testing.T) {
	config, root := t.TempDir(), t.TempDir()
	accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	life := sso.NewLifecycleStore(config)
	const issuer = "https://identity.example.test"
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"one","externalId":"one","userName":"one","active":true,"emails":[{"value":"one@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var desired sso.DirectoryUser
	if err := json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "one-state", Type: "user.updated", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(issuer, ev, desired, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
	u, err := accounts.PublishPreparedSSOUser(context.Background(), "native-one", "native-one", users.RoleUser, issuer, "one", "", "", func() (string, error) {
		a, err := life.ReconcileNativeMailbox(root, issuer, "one", "native-one", "example.test", limits)
		return a.Source, err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &Poller{users: accounts, configDir: config, stateDir: root, stores: map[string]*state.Store{}}
	held, err := p.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(root, sso.NativeRestoreHoldFile)
	if err := os.WriteFile(hold, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.userStore(u.ID); !errors.Is(err, sso.ErrNativeRestoreHold) {
		t.Fatalf("held cached store returned: %v", err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "users", u.ID, "mailbox", "mailbox.db")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := p.userStore(u.ID); err == nil {
		t.Fatal("missing storage used cached state")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("storage recreated: %v", err)
	}
	if _, err := held.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}
