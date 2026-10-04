//go:build linux

package api

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
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func allocateNativeStateTestUser(t *testing.T, srv *Server, issuer, subject string) users.User {
	t.Helper()
	ctx := context.Background()
	domains := srv.nativeDomains
	if _, err := domains.Configure(ctx, "example.test", issuer); err != nil {
		t.Fatal(err)
	}
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := domains.Read()
		return []string{d.RecordValue()}, err
	})
	raw, _ := json.Marshal(map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "id": subject, "externalId": subject, "userName": subject, "active": true, "emails": []map[string]any{{"value": subject + "@example.test", "primary": true}}, "meta": map[string]string{"version": `W/"1"`}})
	var desired sso.DirectoryUser
	if err := json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "native-state-one", Type: "user.updated", At: time.Now()}
	if _, err := srv.ssoLifecycle.ApplyDirectoryUser(issuer, ev, desired, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	u, err := srv.ssoLifecycle.AllocateNativeAccount(ctx, srv.stateDir, issuer, subject, domains, srv.users, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNativeStateAdmissionBeforeCache(t *testing.T) {
	srv := newTestServer(t)
	u := allocateNativeStateTestUser(t, srv, "https://identity.example.test", "one")
	held, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile)
	if err := os.WriteFile(hold, []byte("invalid hold"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func() error{
		"request":     func() error { _, err := srv.userStore(u.ID); return err },
		"maintenance": func() error { _, err := srv.userStoreForMaintenance(u.ID); return err },
	} {
		if err := open(); !errors.Is(err, sso.ErrNativeRestoreHold) {
			t.Fatalf("%s reused held cache: %v", name, err)
		}
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	// This simulates loss after a cache has already been populated.
	missing := filepath.Join(srv.userStateDir(u.ID), "mailbox", "mailbox.db")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.userStore(u.ID); err == nil {
		t.Fatal("missing mailbox reused cached state")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing mailbox recreated: %v", err)
	}
	// Refusal must not close a previously borrowed state handle.
	if _, err := held.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}
