//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Version-1 snapshots (taken before migration) and version-2 snapshots both
// validate; a restored v1 snapshot migrates at next start under the hold.
func TestNativeBackupAcceptsV1AndV2Snapshots(t *testing.T) {
	s, _ := nativeService(t)
	ctx := context.Background()
	keyPath := filepath.Join(s.dirs.Secret, "native-relay.key")
	if _, err := mailmsg.SaveDomainRelay(ctx, filepath.Join(s.dirs.Config, "native-relay.json"), keyPath, mailmsg.DomainRelay{
		Domain: "example.test", Issuer: "https://identity.example.test", Host: "smtp.example.test", Port: 465, Username: "operator", Password: "test-only",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dirs.Config, "native-domain.json"+sso.NativeMigrationCopySuffix), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	collected := func() map[string]bool {
		t.Helper()
		payload, err := s.Collect()
		if err != nil {
			t.Fatal(err)
		}
		paths := map[string]bool{}
		for _, f := range payload.Files {
			paths[f.Path] = true
		}
		return paths
	}
	v2 := collected()
	if !v2["config/"+sso.NativeDomainsFile] || !v2["config/native-domain.json"] || v2["config/native-domain.json"+sso.NativeMigrationCopySuffix] {
		t.Fatal("v2 snapshot must carry the domain set and tombstone, never the v1 copies", v2)
	}
	files := func() map[string][]byte {
		t.Helper()
		out := map[string][]byte{}
		for _, name := range []string{"native-domain.json", sso.NativeDomainsFile, "native-provisioning.json", "native-relay.json"} {
			raw, err := os.ReadFile(filepath.Join(s.dirs.Config, name))
			if err == nil {
				out[name] = raw
			}
		}
		return out
	}
	install := func(set map[string][]byte) {
		t.Helper()
		for _, name := range []string{"native-domain.json", sso.NativeDomainsFile, "native-provisioning.json", "native-relay.json"} {
			path := filepath.Join(s.dirs.Config, name)
			if raw, ok := set[name]; ok {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	v2Files := files()
	if err := sso.WriteNativeV1ForTest(s.dirs.Config, keyPath); err != nil {
		t.Fatal(err)
	}
	v1Files := files()
	// A v1 domain may sit beside a v2 relay (migration crash window); a v2
	// domain beside a v1 relay is no state migration produces.
	mixed := map[string][]byte{"native-domain.json": v1Files["native-domain.json"], "native-provisioning.json": v1Files["native-provisioning.json"], "native-relay.json": v2Files["native-relay.json"]}
	install(mixed)
	collected()
	mixed = map[string][]byte{"native-domain.json": v2Files["native-domain.json"], sso.NativeDomainsFile: v2Files[sso.NativeDomainsFile], "native-provisioning.json": v2Files["native-provisioning.json"], "native-relay.json": v1Files["native-relay.json"]}
	install(mixed)
	if _, err := s.Collect(); err == nil {
		t.Fatal("v2 domain beside a v1 relay accepted")
	}
	install(v1Files)
	if v1 := collected(); v1["config/"+sso.NativeDomainsFile] || !v1["config/native-domain.json"] {
		t.Fatal("v1 snapshot", v1)
	}
	key := pinTestKey(t, s)
	res, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	manifest, _, err := capsule.Open(raw, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range drillChecks(dir, manifest) {
		if !check.Passed {
			t.Error("v1 drill", check.Name)
		}
	}
	if native, err := QuarantineNativeRestore(dir); err != nil || !native {
		t.Fatal("v1 snapshot restore", native, err)
	}
	hold, err := os.ReadFile(filepath.Join(dir, "state", sso.NativeRestoreHoldFile))
	if err != nil {
		t.Fatal(err)
	}
	if migrated, err := sso.MigrateNative(ctx, filepath.Join(dir, "config"), filepath.Join(dir, "private/native-relay.key")); err != nil || !migrated {
		t.Fatal("restored v1 snapshot did not migrate", migrated, err)
	}
	if after, err := os.ReadFile(filepath.Join(dir, "state", sso.NativeRestoreHoldFile)); err != nil || string(after) != string(hold) {
		t.Fatal("migration changed the restore hold", err)
	}
	if _, err := nativeSnapshot(dir); err != nil {
		t.Fatal("migrated restore no longer validates", err)
	}
}

// Configuring a domain initializes the (empty) ledger, so a domain-only
// deployment restores held, like a relay-only one.
func TestNativeDomainOnlyRestoreIsHeld(t *testing.T) {
	s := validService(t)
	if _, err := sso.NewNativeDomainStore(s.dirs.Config).Configure(context.Background(), "example.test", "https://idp.example"); err != nil {
		t.Fatal(err)
	}
	key := pinTestKey(t, s)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restored")
	if _, _, err := capsule.Open(raw, key, dir); err != nil {
		t.Fatal(err)
	}
	if native, err := QuarantineNativeRestore(dir); err != nil || !native {
		t.Fatal("domain-only restore not held", native, err)
	}
}
