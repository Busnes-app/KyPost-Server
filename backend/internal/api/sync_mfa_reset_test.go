package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestDirectoryMFAResetRevokesCredentialsAndOrdersReplay(t *testing.T) {
	for _, multi := range []bool{false, true} {
		name := "single"
		if multi {
			name = "multi"
		}
		t.Run(name, func(t *testing.T) {
			srv := newDirectoryTestServer(t)
			profile := scimUser("member", "member", true)
			if multi {
				makeMultiTenant(t, srv)
				profile = orgUser("member", orgA, true)
			}
			directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "create", 1, profile))
			u, err := srv.users.GetBySSOSubIssuer(srv.ssoStore.Load().IssuerURL, "member")
			if err != nil {
				t.Fatal(err)
			}
			deviceID, _ := pairNativeDevice(t, srv, u.ID, "reset-device")
			store, err := srv.userStore(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.GetOrCreateSubscriberID()
			if err != nil {
				t.Fatal(err)
			}
			hash, err := users.HashPassword(context.Background(), "dav-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.writeDAVPassword(u.ID, davPasswordFile{Hash: hash, CreatedAt: time.Now().Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			seedAgentToken(t, srv, u.ID, state.AgentScopeRead, time.Now())
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			authRequestAs(srv, req, u.ID)
			if _, ok, err := srv.currentUser(req); err != nil || !ok {
				t.Fatal("session fixture", ok, err)
			}
			// A wrong-org reset must neither revoke anything nor consume its event ID.
			if multi {
				wrong := orgUser("member", orgB, true)
				if rec := postDirectory(t, srv, testSyncKey, "user.mfa_reset", "reset", 2, wrong); rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("wrong org: %d %s", rec.Code, rec.Body.String())
				}
				if _, ok, err := srv.currentUser(req); err != nil || !ok {
					t.Fatal("wrong org ended session", err)
				}
				if _, ok := store.GetNativeDevice(deviceID); !ok {
					t.Fatal("wrong org removed device")
				}
			}
			reset := scimUser("member", "member", true)
			if multi {
				reset = orgUser("member", orgA, true)
			}
			if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.mfa_reset", "reset", 2, reset)); got != sso.DirectoryApplied {
				t.Fatal(got)
			}
			if _, ok, err := srv.currentUser(req); err != nil || ok {
				t.Fatal("session survived", ok, err)
			}
			if _, ok := store.GetNativeDevice(deviceID); ok {
				t.Fatal("device survived")
			}
			if _, err := os.Stat(srv.userCardDAVAuthPath(u.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("app password survived", err)
			}
			if tokens, err := srv.globalStore.AgentTokens(context.Background(), u.ID); err != nil || len(tokens) != 0 {
				t.Fatal("agent tokens survived", tokens, err)
			}
			after, err := store.GetOrCreateSubscriberID()
			if err != nil || after == before {
				t.Fatal("pairing authority survived", err)
			}
			kept, err := srv.users.Get(u.ID)
			if err != nil || !kept.Active || kept.SSOLinkRevoked() {
				t.Fatal("reset changed membership", kept, err)
			}
			fence, known, err := srv.ssoLifecycle.Directory(srv.ssoStore.Load().IssuerURL, "member")
			if err != nil || !known || fence.RevokedBefore == 0 || fence.Revision != 2 {
				t.Fatal("token fence", fence, err)
			}
			// A new session and device survive both exact replay and a same-revision retry.
			fresh := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			authRequestAs(srv, fresh, u.ID)
			newDevice, _ := pairNativeDevice(t, srv, u.ID, "new-device")
			for _, id := range []string{"reset", "same-revision"} {
				if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.mfa_reset", id, 2, reset)); got != sso.DirectoryAlreadyApplied {
					t.Fatal(got)
				}
			}
			if _, ok, err := srv.currentUser(fresh); err != nil || !ok {
				t.Fatal("replay ended new session", err)
			}
			if _, ok := store.GetNativeDevice(newDevice); !ok {
				t.Fatal("replay removed new device")
			}
			stale := scimUser("member", "member", true)
			if multi {
				stale = orgUser("member", orgA, true)
			}
			if rec := postDirectory(t, srv, testSyncKey, "user.mfa_reset", "stale-reset", 1, stale); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("stale reset: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
