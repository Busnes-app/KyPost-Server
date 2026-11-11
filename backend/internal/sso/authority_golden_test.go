//go:build linux

package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"
	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// The golden file was written by the code before the authority seam (commit
// c2eef67d). The v3 database must hold the same values after each mutation:
// rendered as the v2 files would have been, the bytes are equal.
// Regenerate only for a deliberate format change: UPDATE_AUTHORITY_GOLDEN=1.
const authorityGolden = "testdata/authority_golden.txt"

var goldenSources = regexp.MustCompile(`native:[0-9a-f]{64}|mbx-[0-9a-f-]{36}`)

func TestAuthorityWritesGoldenValues(t *testing.T) {
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	s := NewLifecycleStore(config)
	at := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	desired := func(sub, address string, revision int, active bool, roles string) {
		raw := fmt.Sprintf(`{"schemas":[%q],"id":%q,"externalId":%q,"userName":%q,"active":%t,"roles":%s,"emails":[{"value":%q,"primary":true}],"meta":{"version":%q}}`, scimUserSchema, sub, sub, sub, active, roles, address, fmt.Sprintf(`W/"%d"`, revision))
		var u DirectoryUser
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		ev := syncauth.Event{ID: fmt.Sprintf("%s-%d", sub, revision), Type: "user.updated", At: at}
		if _, err := s.ApplyDirectoryUser(nativeIssuer, ev, u, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return false, nil }); err != nil {
			t.Fatal(sub, err)
		}
	}
	var out bytes.Buffer
	rendered := t.TempDir()
	names := map[string]string{}
	snapshot := func(step string) {
		fmt.Fprintf(&out, "== %s\n", step)
		life, ledger, err := readAuthorityFile(ctx, filepath.Join(config, NativeAuthorityFile))
		if err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]any{"sso-lifecycle.json": life, nativeProvisioningFile: ledger} {
			if v == (*nativeLedger)(nil) {
				continue
			}
			if err := fsutil.PersistJSONFile(filepath.Join(rendered, name), v); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"sso-lifecycle.json", nativeProvisioningFile} {
			raw, err := os.ReadFile(filepath.Join(rendered, name))
			if os.IsNotExist(err) {
				raw = []byte("(absent)")
			} else if err != nil {
				t.Fatal(err)
			}
			raw = bytes.ReplaceAll(raw, []byte(root), []byte("$ROOT"))
			raw = goldenSources.ReplaceAllFunc(raw, func(b []byte) []byte {
				if names[string(b)] == "" {
					names[string(b)] = fmt.Sprintf("<id%d>", len(names)+1)
				}
				return []byte(names[string(b)])
			})
			fmt.Fprintf(&out, "-- %s\n%s\n", name, raw)
		}
	}
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	desired("one", "one@example.test", 1, true, "[]")
	desired("two", "two@example.test", 1, true, "[]")
	snapshot("directory events before any ledger")
	// Without a domain set the first ledger write is a reconcile commit.
	must(s.ReconcileNativeMailbox(root, nativeIssuer, "one", "local-one", "example.test", nativeLimits))
	snapshot("first reconcile: initialization flag, then ledger")
	domains := provenNativeDomain(t, config)
	if _, err := domains.VerifyDomain(ctx, "example.test"); err != nil {
		t.Fatal(err)
	}
	desired("one", "one@example.test", 3, true, "[]")
	snapshot("domain configured; directory sync activates the address")
	must(s.ReconcileNativeMailbox(root, nativeIssuer, "two", "local-two", "example.test", nativeLimits))
	desired("three", "one@example.test", 1, true, "[]")
	if _, err := s.ReconcileNativeMailbox(root, nativeIssuer, "three", "local-three", "example.test", nativeLimits); err == nil {
		t.Fatal("conflicting primary reconciled")
	}
	snapshot("second reconcile and a recorded address conflict")
	must(s.AddNativeAlias(ctx, root, "local-one", "sales@example.test"))
	must(s.ReleaseNativeAlias(ctx, root, "sales@example.test"))
	must(s.ReassignNativeAddress(ctx, root, "sales@example.test", "local-two"))
	snapshot("alias add, release and reassign")
	box, err := s.CreateNativeMailbox(ctx, root, "local-one", "box@example.test")
	must(box, err)
	must(s.SetNativeMailboxState(ctx, root, box.ID, false))
	snapshot("extra mailbox created and disabled")
	desired("two", "two@example.test", 2, false, "[]")
	desired("one", "one@example.test", 4, true, `["kypost.admin"]`)
	snapshot("deactivation and promotion sync address states")
	if err := s.ReconcileNativeAddresses(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", SessionID: "sid", JWTID: "jti", IssuedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	must(s.RecordSignOnJTI(nativeIssuer, "client", "sign-on", at))
	if err := s.RecordNativeReleaseFloors(nativeIssuer, map[string]NativeReleaseFloor{"two": {Revision: 2}}); err != nil {
		t.Fatal(err)
	}
	snapshot("unchanged reconcile pass, logout, sign-on and release floor")
	raised := nativeLimits
	raised.Records++
	must(MigrateNativeLimits(ctx, config, raised))
	snapshot("limit migration rewrites the ledger without the state rule")
	if os.Getenv("UPDATE_AUTHORITY_GOLDEN") != "" {
		if err := os.WriteFile(authorityGolden, out.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(authorityGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("authority values differ from %s:\n%s", authorityGolden, out.Bytes())
	}
}
