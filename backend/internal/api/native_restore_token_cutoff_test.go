//go:build linux

package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeRestoreTokenCutoffBothSignedSignInPaths(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	ctx := context.Background()
	domains := sso.NewNativeDomainStore(srv.configDir)
	proof, err := domains.Configure(ctx, "example.test", idp.URL())
	if err != nil {
		t.Fatal(err)
	}
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return []string{proof.RecordValue()}, nil })
	if _, err = domains.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"sso-sub-12345","externalId":"sso-sub-12345","userName":"alice","active":true,"emails":[{"value":"alice@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var desired sso.DirectoryUser
	if err = json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "native-create", Type: "user.created", At: time.Now()}
	if _, err = srv.ssoLifecycle.ApplyDirectoryUser(idp.URL(), ev, desired, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = srv.ssoLifecycle.AllocateNativeAccount(ctx, srv.stateDir, idp.URL(), desired.ID, domains, srv.users, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}); err != nil {
		t.Fatal(err)
	}
	// Copy quiescent fixture roots into private stopped staging. The existing
	// server keeps its original roots: this isolates the restored cutoff from
	// the separate hold refusal, without activating or releasing restored data.
	root := t.TempDir()
	if err = os.CopyFS(filepath.Join(root, "config"), os.DirFS(srv.configDir)); err != nil {
		t.Fatal(err)
	}
	if err = os.CopyFS(filepath.Join(root, "state"), os.DirFS(srv.stateDir)); err != nil {
		t.Fatal(err)
	}
	// CopyFS does not preserve the owner-only extraction modes.
	if err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if entry.IsDir() {
			mode = 0700
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	issuance := time.Now().Unix()
	if native, err := backup.QuarantineNativeRestore(root); !native || err != nil {
		t.Fatalf("native=%v err=%v", native, err)
	}
	srv.ssoLifecycle = sso.NewLifecycleStore(filepath.Join(root, "config"))
	for i, issued := range []int64{issuance - 1, issuance, issuance + 30} {
		idp.SetClaims(deviceClaims(map[string]any{"iat": issued, "jti": string(rune('a' + i))}))
		browser := runSSOFlow(t, srv, idp, nil, false)
		if browser.Code != http.StatusForbidden || !strings.Contains(browser.Body.String(), "directory access changed") || sessionCookieFrom(browser) != nil {
			t.Fatalf("browser cutoff status=%d body=%s", browser.Code, browser.Body.String())
		}
		device := httptest.NewRecorder()
		srv.handleNativeSignOn(device, signOnRequest(idp.IDToken()))
		if device.Code != http.StatusForbidden || !strings.Contains(device.Body.String(), "directory access changed") {
			t.Fatalf("device cutoff status=%d body=%s", device.Code, device.Body.String())
		}
	}
	directory, known, err := srv.ssoLifecycle.Directory(idp.URL(), desired.ID)
	if err != nil || !known {
		t.Fatal(err)
	}
	// Use a current post-cutoff issuance; do not fake the server clock or lower
	// the durable cutoff to make a fresh-token control pass.
	wait := time.Until(time.Unix(directory.RevokedBefore, 0))
	if wait > 32*time.Second {
		t.Fatal("unexpected cutoff horizon")
	}
	if wait > 0 {
		time.Sleep(wait)
	}
	idp.SetClaims(deviceClaims(map[string]any{"iat": time.Now().Unix(), "jti": "fresh-after-cutoff"}))
	if browser := runSSOFlow(t, srv, idp, nil, false); browser.Code != http.StatusFound {
		t.Fatalf("fresh browser %d: %s", browser.Code, browser.Body.String())
	}
	device := httptest.NewRecorder()
	srv.handleNativeSignOn(device, signOnRequest(idp.IDToken()))
	if device.Code != http.StatusOK {
		t.Fatalf("fresh device %d: %s", device.Code, device.Body.String())
	}
	if sso.RequireNativeRestoreReleased(filepath.Join(root, "state")) == nil {
		t.Fatal("test released staged restore")
	}
}
