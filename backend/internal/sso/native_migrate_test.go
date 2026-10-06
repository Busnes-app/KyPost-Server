//go:build linux

package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// v1Fixture is a migrated-from deployment: a non-admin subject whose directory
// revision (3) is newer than its reservation (1), an administrator subject with
// a mailbox, and a relay, all written in the version-1 formats.
func v1Fixture(t *testing.T) (config, keyPath string, ids map[string]string) {
	t.Helper()
	ctx := context.Background()
	config, root := t.TempDir(), t.TempDir()
	keyPath = filepath.Join(t.TempDir(), "native-relay.key")
	accounts, err := users.LoadOrMigrate(ctx, config, filepath.Join(config, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	life := NewLifecycleStore(config)
	domains := provenNativeDomain(t, config)
	ids = map[string]string{}
	nativeDesired(t, life, "one", "one@example.test", 1, true)
	// v1 gave administrators mailboxes; reproduce one by promoting after allocation.
	nativeDesired(t, life, "boss", "boss@example.test", 1, true)
	for _, subject := range []string{"one", "boss"} {
		u, err := life.AllocateNativeAccount(ctx, root, nativeIssuer, subject, domains, accounts, nativeLimits)
		if err != nil {
			t.Fatal(err)
		}
		ids[subject] = u.ID
	}
	if _, err := accounts.SetRole(ids["boss"], users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"schemas":[%q],"id":"boss","externalId":"boss","userName":"boss","active":true,"roles":[{"value":%q}],"emails":[{"value":"boss@example.test","primary":true}],"meta":{"version":"W/\"2\""}}`, scimUserSchema, AdminAppRole)
	var admin DirectoryUser
	if err := json.Unmarshal([]byte(raw), &admin); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "boss-2", Type: "user.updated", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(nativeIssuer, ev, admin, EventDigest(ev.Type, []byte(raw)), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	nativeDesired(t, life, "one", "one@example.test", 3, true)
	if _, err := mailmsg.SaveDomainRelay(ctx, filepath.Join(config, "native-relay.json"), keyPath, mailmsg.DomainRelay{Domain: "example.test", Issuer: nativeIssuer, Host: "smtp.example.test", Port: 465, Username: "operator", Password: "test-only"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteNativeV1ForTest(config, keyPath); err != nil {
		t.Fatal(err)
	}
	return config, keyPath, ids
}

// configFiles maps every non-lock file to its bytes; the sealed relay (random
// nonce) is compared by its decrypted content and version instead.
func configFiles(t *testing.T, config, keyPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(config, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == "native-relay.json" {
			relay, version, err := mailmsg.ReadDomainRelayAnyVersion(filepath.Join(config, e.Name()), keyPath)
			raw = fmt.Appendf(nil, "%d %+v %v", version, relay, err)
		}
		out[e.Name()] = string(raw)
	}
	return out
}

func copyConfig(t *testing.T, from string) string {
	t.Helper()
	to := t.TempDir()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return to
}

func TestNativeMigrationLedgerRelayAndIdempotence(t *testing.T) {
	ctx := context.Background()
	config, keyPath, ids := v1Fixture(t)
	v1Ledger, err := os.ReadFile(filepath.Join(config, nativeProvisioningFile))
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := parseNativeLedger(v1Ledger, true)
	if err != nil {
		t.Fatal(err)
	}
	legacy := want.Accounts[directoryKey(nativeIssuer, "boss")]
	legacy.LegacyMixedUse = true // the only field migration adds to the view
	want.Accounts[directoryKey(nativeIssuer, "boss")] = legacy
	relayV1, version, err := mailmsg.ReadDomainRelayAnyVersion(filepath.Join(config, "native-relay.json"), keyPath)
	if err != nil || version != 1 {
		t.Fatal("fixture relay is not version 1", version, err)
	}
	migrated, err := MigrateNative(ctx, config, keyPath)
	if err != nil || !migrated {
		t.Fatal(migrated, err)
	}
	life := NewLifecycleStore(config)
	got, err := life.loadNative()
	if err != nil || !maps.Equal(got.Accounts, want.Accounts) {
		t.Fatalf("assignments changed: %v\n%+v\n%+v", err, got.Accounts, want.Accounts)
	}
	relay, exists, err := mailmsg.ReadDomainRelay(filepath.Join(config, "native-relay.json"), keyPath)
	if err != nil || !exists || relay != relayV1 {
		t.Fatal("relay generation or profile changed", relay, relayV1, err)
	}
	raw, err := os.ReadFile(filepath.Join(config, nativeProvisioningFile))
	if err != nil {
		t.Fatal(err)
	}
	var l nativeLedger
	if err := json.Unmarshal(raw, &l); err != nil || l.Version != 2 {
		t.Fatal("ledger not version 2", err)
	}
	one, boss := l.Addresses["one@example.test"], l.Addresses["boss@example.test"]
	if one.Generation != 3 || boss.Generation != 2 || one.Mailbox != ids["one"] || len(one.History) != 1 || one.History[0] != (nativeAddressHistory{Mailbox: ids["one"], Generation: 1}) {
		t.Fatalf("generation not seeded from the directory revision: %+v %+v", one, boss)
	}
	if l.Accounts[directoryKey(nativeIssuer, "one")].LegacyMixedUse || !l.Accounts[directoryKey(nativeIssuer, "boss")].LegacyMixedUse {
		t.Fatal("legacyMixedUse must mark exactly the administrator subject")
	}
	if m := l.Mailboxes[ids["one"]]; m.Kind != "primary" || m.State != "active" || m.Owner.Subject != "one" {
		t.Fatalf("primary mailbox: %+v", m)
	}
	for _, name := range []string{legacyNativeDomainFile, nativeProvisioningFile, "native-relay.json"} {
		if _, err := os.Stat(filepath.Join(config, name+NativeMigrationCopySuffix)); err != nil {
			t.Fatal("v1 source not preserved", name, err)
		}
	}
	// A later save keeps the flags and never lowers a generation.
	if err := life.saveNative(got); err != nil {
		t.Fatal(err)
	}
	if again, err := os.ReadFile(filepath.Join(config, nativeProvisioningFile)); err != nil || !bytes.Equal(again, raw) {
		t.Fatal("re-save changed the ledger", err)
	}
	before := configFiles(t, config, keyPath)
	if migrated, err := MigrateNative(ctx, config, keyPath); err != nil || migrated {
		t.Fatal("second run was not a no-op", migrated, err)
	}
	if after := configFiles(t, config, keyPath); !maps.Equal(before, after) {
		t.Fatal("second run changed files")
	}
}

func TestNativeMigrationCrashAndV1RollbackConverge(t *testing.T) {
	ctx := context.Background()
	config, keyPath, _ := v1Fixture(t)
	clean := copyConfig(t, config)
	if _, err := MigrateNative(ctx, clean, keyPath); err != nil {
		t.Fatal(err)
	}
	want := configFiles(t, clean, keyPath)
	crash := errors.New("simulated crash")
	stopAfter := func(k int) {
		nativeMigrationStep = func(step int) error {
			if step == k {
				return crash
			}
			return nil
		}
	}
	t.Cleanup(func() { nativeMigrationStep = func(int) error { return nil } })
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprint("step-", k), func(t *testing.T) {
			dir := copyConfig(t, config)
			stopAfter(k)
			if _, err := MigrateNative(ctx, dir, keyPath); !errors.Is(err, crash) {
				t.Fatal(err)
			}
			if _, err := NewNativeDomainStore(dir).Read(); !errors.Is(err, ErrNativeMigration) {
				t.Fatal("half-migrated storage admitted", err)
			}
			stopAfter(0)
			if _, err := MigrateNative(ctx, dir, keyPath); err != nil {
				t.Fatal(err)
			}
			if got := configFiles(t, dir, keyPath); !maps.Equal(got, want) {
				t.Fatalf("resumed migration diverged from a clean run:\n%v\n%v", got, want)
			}
		})
	}
	// A v1 binary between runs rewrites its domain and ledger; the re-run
	// re-copies them and converges on the new v1 data.
	dir := copyConfig(t, config)
	stopAfter(2)
	if _, err := MigrateNative(ctx, dir, keyPath); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	stopAfter(0)
	d, err := parseLegacyNativeDomain(mustRead(t, filepath.Join(dir, legacyNativeDomainFile)))
	if err != nil {
		t.Fatal(err)
	}
	d.Token = strings.Repeat("ab", 32)
	if err := fsutil.PersistJSONFile(filepath.Join(dir, legacyNativeDomainFile), d); err != nil {
		t.Fatal(err)
	}
	f, _, err := parseNativeLedger(mustRead(t, filepath.Join(dir, nativeProvisioningFile)), true)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Accounts[directoryKey(nativeIssuer, "one")]
	a.Status, a.Failure = "failed", "storage_conflict_or_unavailable"
	f.Accounts[directoryKey(nativeIssuer, "one")] = a
	if err := fsutil.PersistJSONFile(filepath.Join(dir, nativeProvisioningFile), map[string]any{"version": 1, "accounts": f.Accounts}); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, dir, keyPath); err != nil {
		t.Fatal(err)
	}
	if got, err := NewNativeDomainStore(dir).Read(); err != nil || got.Token != d.Token {
		t.Fatal("rolled-back domain not converged", got, err)
	}
	if got, _, err := NewLifecycleStore(dir).NativeAssignment(nativeIssuer, "one"); err != nil || got != a {
		t.Fatal("rolled-back ledger not converged", got, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNativeMigrationV1BinaryFailsClosed(t *testing.T) {
	config, keyPath, _ := v1Fixture(t)
	if _, err := MigrateNative(context.Background(), config, keyPath); err != nil {
		t.Fatal(err)
	}
	// v1 Read unmarshalled native-domain.json into NativeDomain and applied this
	// rule; the tombstone has no token, so Configure/Verify/receiving refuse.
	var v1 NativeDomain
	if err := json.Unmarshal(mustRead(t, filepath.Join(config, legacyNativeDomainFile)), &v1); err != nil || validNativeDomain(v1) {
		t.Fatal("v1 domain rule accepts the tombstone", err)
	}
	// v1 required ledger version 1 and relay version 1.
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(config, nativeProvisioningFile)), &head); err != nil || head.Version != 2 {
		t.Fatal("ledger readable by v1", head, err)
	}
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, ok := cryptutil.ParseEnvelope(mustRead(t, filepath.Join(config, "native-relay.json")))
	plain, err := cryptutil.Open(envelope, key)
	if !ok || err != nil {
		t.Fatal(err)
	}
	var relay map[string]any
	if err := json.Unmarshal(plain, &relay); err != nil || relay["version"] != float64(2) || relay["domain"] != nil || fmt.Sprint(relay["domains"]) != "[example.test]" || fmt.Sprint(relay["retiredDomains"]) != "[]" {
		t.Fatal("relay readable by v1 or wrong shape", relay, err)
	}
}

func TestNativeMigrationFreshInstallAndConfigure(t *testing.T) {
	ctx := context.Background()
	config := t.TempDir()
	if migrated, err := MigrateNative(ctx, config, filepath.Join(config, "absent.key")); err != nil || migrated {
		t.Fatal(migrated, err)
	}
	if entries, _ := os.ReadDir(config); len(entries) != 0 {
		t.Fatal("fresh migration created files", entries)
	}
	s := NewNativeDomainStore(config)
	if d, err := s.Read(); err != nil || d.Domain != "" {
		t.Fatal("fresh install is not unconfigured", d, err)
	}
	if _, err := s.Configure(ctx, "example.test", nativeIssuer); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(config, legacyNativeDomainFile))); got != "{\n  \"migratedTo\": \"native-domains.json\"\n}" {
		t.Fatal("tombstone", got)
	}
	if got := string(mustRead(t, filepath.Join(config, nativeProvisioningFile))); got != "{\n  \"version\": 2,\n  \"accounts\": {},\n  \"mailboxes\": {},\n  \"addresses\": {}\n}" {
		t.Fatal("empty ledger", got)
	}
	var set nativeDomainSet
	if err := json.Unmarshal(mustRead(t, s.path), &set); err != nil || set.Version != 1 || set.Issuer != nativeIssuer || len(set.Domains) != 1 || set.Retired == nil || len(set.Retired) != 0 {
		t.Fatal("domain set", set, err)
	}
	lifecycle, err := NewLifecycleStore(config).load()
	if err != nil || !lifecycle.NativeProvisioningInitialized {
		t.Fatal("ledger not initialized", err)
	}
	if migrated, err := MigrateNative(ctx, config, filepath.Join(config, "absent.key")); err != nil || migrated {
		t.Fatal("migration touched a v2 install", migrated, err)
	}
	// Native allocation then starts at once (outboundFixture and every
	// allocation test run through this Configure path).

	// Configure crashing between the set and the tombstone: migration finishes
	// the tombstone, so a v1 binary cannot configure behind the set.
	if err := os.Remove(filepath.Join(config, legacyNativeDomainFile)); err != nil {
		t.Fatal(err)
	}
	if migrated, err := MigrateNative(ctx, config, filepath.Join(config, "absent.key")); err != nil || migrated {
		t.Fatal(migrated, err)
	}
	if tombstone, _, err := legacyNativeDomain(config); err != nil || !tombstone {
		t.Fatal("tombstone not restored after Configure crash window", err)
	}
}

func TestNativeMigrationRefusals(t *testing.T) {
	ctx := context.Background()
	config, keyPath, _ := v1Fixture(t)
	s, life := NewNativeDomainStore(config), NewLifecycleStore(config)
	if _, err := s.Read(); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("v1 domain admitted", err)
	}
	if _, err := s.Configure(ctx, "example.test", nativeIssuer); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("configure behind unmigrated v1 data", err)
	}
	if _, _, err := life.NativeAssignment(nativeIssuer, "one"); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("v1 ledger admitted", err)
	}
	if _, _, err := mailmsg.ReadDomainRelay(filepath.Join(config, "native-relay.json"), keyPath); !errors.Is(err, mailmsg.ErrDomainRelay) {
		t.Fatal("v1 relay admitted at runtime", err)
	}
	// v1 refused a ledger behind an unset initialization fence; migration must
	// not grant it authority, and writes nothing but the preserved copies.
	unfenced := copyConfig(t, config)
	unfencedLife := NewLifecycleStore(unfenced)
	lifecycle, err := unfencedLife.load()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.NativeProvisioningInitialized = false
	if err := fsutil.PersistJSONFile(unfencedLife.path, lifecycle); err != nil {
		t.Fatal(err)
	}
	before := configFiles(t, unfenced, keyPath)
	if _, err := MigrateNative(ctx, unfenced, keyPath); !errors.Is(err, ErrNativeProvisioning) {
		t.Fatal("unfenced v1 ledger migrated", err)
	}
	after := configFiles(t, unfenced, keyPath)
	for name := range after {
		if before[name] != after[name] && !strings.HasSuffix(name, NativeMigrationCopySuffix) {
			t.Fatal("refused migration wrote", name)
		}
	}
	if _, err := NewNativeDomainStore(unfenced).Read(); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("refused migration admitted native", err)
	}
	// A failed migration leaves a v1 ledger; Configure must not adopt it, and
	// the migration recovers once the cause (a lost domain file) is fixed.
	lost := copyConfig(t, config)
	domainBytes := mustRead(t, filepath.Join(lost, legacyNativeDomainFile))
	if err := os.Remove(filepath.Join(lost, legacyNativeDomainFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, lost, keyPath); err == nil {
		t.Fatal("v1 ledger without a domain migrated")
	}
	if _, err := NewNativeDomainStore(lost).Configure(ctx, "example.test", nativeIssuer); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("configure adopted a v1 ledger", err)
	}
	if err := os.WriteFile(filepath.Join(lost, legacyNativeDomainFile), domainBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if migrated, err := MigrateNative(ctx, lost, keyPath); err != nil || !migrated {
		t.Fatal("migration did not recover", migrated, err)
	}
	if _, err := NewNativeDomainStore(lost).Read(); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, config, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Configure(ctx, "other.test", nativeIssuer); !errors.Is(err, ErrNativeDomain) {
		t.Fatal("second domain accepted", err)
	}
	for _, missing := range []string{nativeProvisioningFile, NativeDomainsFile} {
		dir := copyConfig(t, config)
		if err := os.Remove(filepath.Join(dir, missing)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewNativeDomainStore(dir).Read(); !errors.Is(err, ErrNativeMigration) {
			t.Fatal("tombstone without", missing, err)
		}
	}
	// Unparseable v1 data fails before the tombstone, so native stays refused.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, legacyNativeDomainFile), []byte(`{"domain":"example.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, broken, keyPath); err == nil {
		t.Fatal("invalid v1 domain migrated")
	}
	if _, err := NewNativeDomainStore(broken).Read(); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("failed migration admitted native", err)
	}
	orphan := t.TempDir()
	if err := os.WriteFile(filepath.Join(orphan, nativeProvisioningFile), []byte(`{"version":1,"accounts":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, orphan, keyPath); err == nil {
		t.Fatal("v1 ledger without a domain migrated")
	}

	// Rollback restore into old volumes: a backup without the relay is copied
	// over v2 files, leaving the earlier relay copy behind. Migration must not
	// resurrect that relay or its credential.
	rolled := copyConfig(t, config)
	if _, err := MigrateNative(ctx, rolled, keyPath); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{legacyNativeDomainFile, nativeProvisioningFile} {
		if err := os.WriteFile(filepath.Join(rolled, name), mustRead(t, filepath.Join(rolled, name+NativeMigrationCopySuffix)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(rolled, "native-relay.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, rolled, keyPath); err == nil {
		t.Fatal("leftover relay copy used without its source")
	}
	if _, err := os.Lstat(filepath.Join(rolled, "native-relay.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("relay resurrected from a leftover copy", err)
	}
	// A relay bound to another domain or issuer is refused before any write.
	mismatched, mismatchedKey, _ := v1Fixture(t)
	var d map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(mismatched, legacyNativeDomainFile)), &d); err != nil {
		t.Fatal(err)
	}
	d["domain"] = "other.test"
	if err := fsutil.PersistJSONFile(filepath.Join(mismatched, legacyNativeDomainFile), d); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateNative(ctx, mismatched, mismatchedKey); err == nil || !strings.Contains(err.Error(), "native-relay.json") {
		t.Fatal("relay for another domain migrated", err)
	}
	if _, err := os.Lstat(filepath.Join(mismatched, NativeDomainsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("output written before relay validation", err)
	}
}

// The recovery digest hashes the single-mailbox view, which migration changes
// only by flagging administrator subjects, and migration leaves a restore hold
// untouched.
func TestNativeMigrationKeepsRecoveryDigestAndHold(t *testing.T) {
	life, dir, settings, key, challenge := recoveryFixture(t)
	if _, err := NewNativeDomainStore(dir).Configure(context.Background(), "example.test", settings.IssuerURL); err != nil {
		t.Fatal(err)
	}
	hold := mustRead(t, filepath.Join(dir, NativeRestoreHoldFile))
	keyPath := filepath.Join(dir, "absent-relay.key")
	if err := WriteNativeV1ForTest(dir, keyPath); err != nil {
		t.Fatal(err)
	}
	if migrated, err := MigrateNative(context.Background(), dir, keyPath); err != nil || !migrated {
		t.Fatal(migrated, err)
	}
	if !bytes.Equal(hold, mustRead(t, filepath.Join(dir, NativeRestoreHoldFile))) {
		t.Fatal("migration changed the restore hold")
	}
	c, _, _, err := life.nativeRecoveryInputs(dir, settings, key, nil)
	if err != nil || c.AuthorityDigest != challenge.AuthorityDigest {
		t.Fatal("recovery authority digest changed", err)
	}
}

// A job queued under v1 keeps its relay generation and directory revision, so
// after migration the next claim passes authority instead of ending stale.
func TestNativeMigrationKeepsQueuedOutboxJob(t *testing.T) {
	ctx := context.Background()
	sender, u, job := outboundFixture(t)
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Queue(ctx, u.ID, id, job); err != nil {
		t.Fatal(err)
	}
	if err = WriteNativeV1ForTest(sender.ConfigDir, sender.keyPath()); err != nil {
		t.Fatal(err)
	}
	if _, err = sender.Submit(ctx, u.ID, id, 0); err == nil {
		t.Fatal("unmigrated storage submitted")
	}
	if migrated, err := MigrateNative(ctx, sender.ConfigDir, sender.keyPath()); err != nil || !migrated {
		t.Fatal(migrated, err)
	}
	result, err := sender.Submit(ctx, u.ID, id, 0)
	statuses, _, statusErr := sender.Status(ctx, u.ID, id)
	if errors.Is(err, ErrNativeOutboundStale) || statusErr != nil || len(statuses) != 1 || statuses[0].Attempts != 1 {
		t.Fatalf("queued job did not reach a claim after migration: result=%+v err=%v statuses=%+v statusErr=%v", result, err, statuses, statusErr)
	}
}

// Snapshots may mix formats only as a migration crash leaves them.
func TestNativeSnapshotFormatMixes(t *testing.T) {
	v1, keyPath, _ := v1Fixture(t)
	v2 := copyConfig(t, v1)
	if _, err := MigrateNative(context.Background(), v2, keyPath); err != nil {
		t.Fatal(err)
	}
	ledger, _, err := parseNativeLedger(mustRead(t, filepath.Join(v1, nativeProvisioningFile)), true)
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	for _, a := range ledger.Accounts {
		root = a.StateRoot
	}
	var doc struct {
		Users []users.User `json:"users"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(v1, "users.json")), &doc); err != nil {
		t.Fatal(err)
	}
	put := func(dir, name, from string) {
		t.Helper()
		if from == "" {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), mustRead(t, filepath.Join(from, name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Configure's crash window: the empty ledger is written before the set.
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, nativeProvisioningFile), []byte(`{"version":2,"accounts":{},"mailboxes":{},"addresses":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                string
		domain, set, ledger string
		pass                bool
		unprovisioned       bool // no native users yet, as in Configure's window
	}{
		{"no-domain-empty-v2-ledger", "", "", empty, true, true},
		{"all-v1", v1, "", v1, true, false},
		{"all-v2", v2, v2, v2, true, false},
		{"v1-domain-v2-ledger", v1, v2, v2, true, false},
		{"v2-domain-v1-ledger", v2, v2, v1, false, false},
		{"no-domain-v1-ledger", "", "", v1, false, false},
		{"no-domain-v2-ledger", "", "", v2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyConfig(t, v1)
			put(dir, legacyNativeDomainFile, tc.domain)
			put(dir, NativeDomainsFile, tc.set)
			put(dir, nativeProvisioningFile, tc.ledger)
			list, stateRoot := doc.Users, root
			if tc.unprovisioned {
				list, stateRoot = nil, t.TempDir()
			}
			if _, err := NewLifecycleStore(dir).ValidateNativeSnapshot(stateRoot, list); (err == nil) != tc.pass {
				t.Fatalf("pass=%v err=%v", tc.pass, err)
			}
		})
	}
}
