//go:build linux

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func TestNativeCardDAVRestoreHoldRejectsColdAndCachedCredentials(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "cold"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("STATE_DIR", t.TempDir())
			s := newNativeRuntimeServer(t)
			directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "dav-create", 1, runtimeDirectoryUser(true)))
			u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
			if err != nil || u.NativeMailboxSource == "" {
				t.Fatal("native fixture not provisioned", err)
			}
			legacy, err := s.users.Create(context.Background(), "legacy-dav", "legacy-login-password", users.RoleUser)
			if err != nil {
				t.Fatal(err)
			}
			const secret = "historical-carddav-password"
			hash, err := users.HashPassword(context.Background(), secret)
			if err != nil {
				t.Fatal(err)
			}
			for _, user := range []users.User{u, legacy} {
				if err := s.writeDAVPassword(user.ID, davPasswordFile{Hash: hash, CreatedAt: "2026-10-03T00:00:00Z"}); err != nil {
					t.Fatal(err)
				}
			}
			request := func(user users.User) int {
				r := httptest.NewRequest("PROPFIND", davPrefix+"/"+user.Username+"/", strings.NewReader(`<D:propfind xmlns:D="DAV:"><D:prop><D:displayname/></D:prop></D:propfind>`))
				r.Header.Set("Depth", "0")
				r.Header.Set("Content-Type", "application/xml")
				r.SetBasicAuth(user.Username, secret)
				w := httptest.NewRecorder()
				s.routes().ServeHTTP(w, r)
				if w.Code == http.StatusBadRequest {
					t.Logf("CardDAV refusal: %s", w.Body.String())
				}
				return w.Code
			}
			if got := request(u); got != http.StatusMultiStatus {
				t.Fatalf("live native CardDAV: %d", got)
			}
			if _, ok := s.davCredentials.get(u.Username, secret); !ok {
				t.Fatal("control did not warm actual credential cache")
			}
			if !cached {
				s.davCredentials.invalidateUser(u.Username)
			}
			// Even malformed recovery evidence must close admission.
			if err := os.WriteFile(filepath.Join(s.stateDir, sso.NativeRestoreHoldFile), []byte("malformed hold"), 0600); err != nil {
				t.Fatal(err)
			}
			if got := request(u); got != http.StatusUnauthorized {
				t.Fatalf("held native CardDAV admitted historical credential: %d", got)
			}
			if got := request(legacy); got != http.StatusMultiStatus {
				t.Fatalf("native hold blocked legacy CardDAV: %d", got)
			}
			if f, exists, err := s.readDAVPassword(u.ID); err != nil || !exists || f.Hash != hash {
				t.Fatal("runtime hold mutated credential file", err)
			}
		})
	}
}
