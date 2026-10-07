package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// NativeMigrationCopySuffix marks the preserved version-1 sources.
const NativeMigrationCopySuffix = ".v1-migrated"

// nativeMigrationStep lets tests stop the migration after a step, as a crash would.
var nativeMigrationStep = func(int) error { return nil }

// MigrateNative converts version-1 native domain, ledger and relay files to
// version 2 in place, under domain -> directory -> users locks. Every output
// derives only from the preserved *.v1-migrated copies and is overwritten, so
// a crash, or a v1 binary that rewrote v1 files in between, converges on
// re-run. The tombstone is written last; once present this is a no-op. It
// grants no authority and leaves a restore hold in place.
func MigrateNative(ctx context.Context, configDir, relayKeyPath string) (bool, error) {
	tombstone, _, err := legacyNativeDomain(configDir)
	if err != nil || tombstone {
		return false, err
	}
	sources := []string{legacyNativeDomainFile, nativeProvisioningFile, "native-relay.json"}
	if !anyExists(configDir, sources) {
		return false, nil // fresh install or never configured: starts in v2
	}
	for _, lock := range []string{NativeDomainsFile, "sso-lifecycle.json", "users.json"} {
		release, err := fsutil.LockFileContext(ctx, filepath.Join(configDir, lock))
		if err != nil {
			return false, err
		}
		defer release()
	}
	if tombstone, _, err = legacyNativeDomain(configDir); err != nil || tombstone {
		return false, err
	}
	// 1. Preserve every source that still holds version-1 data.
	for _, name := range sources {
		if err := preserveNativeSource(configDir, name, relayKeyPath); err != nil {
			return false, fmt.Errorf("preserve %s: %w", name, err)
		}
	}
	if err := nativeMigrationStep(1); err != nil {
		return false, err
	}
	copyOf := func(name string) ([]byte, bool, error) {
		raw, err := os.ReadFile(filepath.Join(configDir, name+NativeMigrationCopySuffix))
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return raw, err == nil, err
	}
	domainRaw, hasDomain, err := copyOf(legacyNativeDomainFile)
	if err != nil {
		return false, err
	}
	ledgerRaw, hasLedger, err := copyOf(nativeProvisioningFile)
	if err != nil {
		return false, err
	}
	relayRaw, hasRelay, err := copyOf("native-relay.json")
	if err != nil {
		return false, err
	}
	if !hasDomain {
		if hasLedger || hasRelay {
			return false, errors.New("version-1 ledger or relay without a mail domain; restore a consistent backup")
		}
		// Only v2 files: Configure crashed between the set and the tombstone.
		// Finish it so a v1 binary cannot configure a domain behind the set.
		if _, err := os.Lstat(filepath.Join(configDir, NativeDomainsFile)); err == nil {
			return false, writeNativeDomainTombstone(configDir)
		}
		return false, nil
	}
	// Validate every copy before the first output write.
	domain, err := parseLegacyNativeDomain(domainRaw)
	if err != nil {
		return false, fmt.Errorf("native-domain.json: %w", err)
	}
	life := NewLifecycleStore(configDir)
	var ledger nativeAssignments
	if hasLedger {
		if ledger, err = life.migratedNativeLedger(ledgerRaw); err != nil {
			return false, fmt.Errorf("native-provisioning.json: %w", err)
		}
	}
	if hasRelay {
		if err := nativeRelayCopyMatches(relayKeyPath, relayRaw, domain); err != nil {
			return false, fmt.Errorf("native-relay.json: %w", err)
		}
	}
	// 2. Domain set.
	store := NewNativeDomainStore(configDir)
	set := nativeDomainSet{Version: 1, Issuer: domain.Issuer, Domains: map[string]nativeDomainProof{domain.Domain: {domain.Token, domain.ExpiresAt, domain.Established, domain.VerifiedUntil}}, Retired: []string{}}
	if err := fsutil.PersistJSONFile(store.path, set); err != nil {
		return false, err
	}
	if err := nativeMigrationStep(2); err != nil {
		return false, err
	}
	// 3. Ledger, or an empty one when a domain exists without a ledger.
	if hasLedger {
		if err := life.saveNative(ledger); err != nil {
			return false, fmt.Errorf("native-provisioning.json: %w", err)
		}
	} else if err := life.ensureNativeLedgerLocked(); err != nil {
		return false, fmt.Errorf("native-provisioning.json: %w", err)
	}
	if err := nativeMigrationStep(3); err != nil {
		return false, err
	}
	// 4. Relay, keeping Generation so queued and retrying jobs stay valid.
	if hasRelay {
		if err := migrateNativeRelay(filepath.Join(configDir, "native-relay.json"), relayKeyPath, relayRaw); err != nil {
			return false, fmt.Errorf("native-relay.json: %w", err)
		}
	}
	if err := nativeMigrationStep(4); err != nil {
		return false, err
	}
	// 5. Tombstone last.
	return true, writeNativeDomainTombstone(configDir)
}

func anyExists(dir string, names []string) bool {
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// preserveNativeSource re-copies a source while it holds version-1 data and
// keeps the existing copy once the source has been converted or tombstoned.
func preserveNativeSource(configDir, name, relayKeyPath string) error {
	path := filepath.Join(configDir, name)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Migration never deletes a source, so a copy without one is left over
		// from an earlier upgrade (e.g. a rollback restore into old volumes).
		// Using it would resurrect state, such as relay credentials, that the
		// restored backup does not contain.
		if _, err := os.Lstat(path + NativeMigrationCopySuffix); err == nil {
			return fmt.Errorf("%s is missing but %s%s is left over from an earlier migration; move that copy aside and restart", name, name, NativeMigrationCopySuffix)
		}
		return nil
	}
	if err != nil {
		return err
	}
	version := 0
	switch name {
	case legacyNativeDomainFile:
		version = 1 // checked by the caller: not the tombstone
	case nativeProvisioningFile:
		var head struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return err
		}
		version = head.Version
	default:
		key, err := cryptutil.LoadKey(relayKeyPath)
		if err != nil {
			return mailmsg.ErrDomainRelay
		}
		if _, version, err = mailmsg.DecodeDomainRelay(raw, key); err != nil {
			return err
		}
	}
	if version != 1 {
		return nil
	}
	return fsutil.AtomicWriteFile(path+NativeMigrationCopySuffix, raw, 0600)
}

// migratedNativeLedger converts the version-1 copy. Each primary address
// generation is later seeded from the subject's directory revision, which bounds
// every route and binding generation v1 wrote, so older pending bindings still
// quarantine and current ones still import. Administrator subjects that already
// hold a mailbox get legacyMixedUse. A ledger behind an unset initialization
// fence was refused by v1 and must not gain authority here.
func (s *LifecycleStore) migratedNativeLedger(raw []byte) (nativeAssignments, error) {
	f, version, err := parseNativeLedger(raw, true)
	if err != nil || version != 1 {
		return nativeAssignments{}, ErrNativeProvisioning
	}
	lifecycle, err := s.load()
	if err != nil {
		return nativeAssignments{}, err
	}
	if !lifecycle.NativeProvisioningInitialized {
		return nativeAssignments{}, ErrNativeProvisioning
	}
	for key, a := range f.Accounts {
		d := lifecycle.Directory[key]
		a.LegacyMixedUse = d.Resource != nil && HasAdminRole(d.Resource.Roles)
		f.Accounts[key] = a
	}
	return f, nil
}

// nativeRelayCopyMatches refuses a relay copy bound to another domain or issuer.
func nativeRelayCopyMatches(keyPath string, raw []byte, domain NativeDomain) error {
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		return mailmsg.ErrDomainRelay
	}
	relay, version, err := mailmsg.DecodeDomainRelay(raw, key)
	if err != nil || version != 1 || !slices.Equal(relay.Domains, []string{domain.Domain}) || relay.Issuer != domain.Issuer {
		return mailmsg.ErrDomainRelay
	}
	return nil
}

// migrateNativeRelay rewrites the relay only when its content would change, so
// a re-run leaves the sealed bytes untouched.
func migrateNativeRelay(path, keyPath string, v1 []byte) error {
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		return mailmsg.ErrDomainRelay
	}
	relay, version, err := mailmsg.DecodeDomainRelay(v1, key)
	if err != nil || version != 1 {
		return mailmsg.ErrDomainRelay
	}
	if current, version, err := mailmsg.ReadDomainRelayAnyVersion(path, keyPath); err == nil && version == 2 && current.Equal(relay) {
		return nil
	}
	sealed, err := mailmsg.SealDomainRelay(relay, key)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, sealed, 0600)
}

// WriteNativeV1ForTest rewrites the current native files exactly as a v1
// binary stored them, so migration and backup tests can start from v1 state.
func WriteNativeV1ForTest(configDir, relayKeyPath string) error {
	if !testing.Testing() {
		panic("version-1 native fixture outside tests")
	}
	d, err := NewNativeDomainStore(configDir).Read()
	if err != nil {
		return err
	}
	f, err := NewLifecycleStore(configDir).loadNative()
	if err != nil {
		return err
	}
	relayPath := filepath.Join(configDir, "native-relay.json")
	relay, version, err := mailmsg.ReadDomainRelayAnyVersion(relayPath, relayKeyPath)
	if err != nil {
		return err
	}
	if version != 0 {
		key, err := cryptutil.LoadKey(relayKeyPath)
		if err != nil {
			return err
		}
		plain, err := json.Marshal(map[string]any{"version": 1, "generation": relay.Generation, "domain": relay.Domains[0], "issuer": relay.Issuer, "host": relay.Host, "port": relay.Port, "smtpUsername": relay.Username, "smtpPassword": relay.Password})
		if err != nil {
			return err
		}
		envelope, err := cryptutil.Seal(plain, key)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		if err := fsutil.AtomicWriteFile(relayPath, raw, 0600); err != nil {
			return err
		}
	}
	if err := fsutil.PersistJSONFile(filepath.Join(configDir, nativeProvisioningFile), map[string]any{"version": 1, "accounts": f.Accounts}); err != nil {
		return err
	}
	if err := fsutil.PersistJSONFile(filepath.Join(configDir, legacyNativeDomainFile), d); err != nil {
		return err
	}
	return os.Remove(filepath.Join(configDir, NativeDomainsFile))
}
