package sso

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// NativeAuthorityCopySuffix marks the preserved v2 sources of the v3 migration.
const NativeAuthorityCopySuffix = ".v2-migrated"

// authorityMigrationStep lets tests stop MigrateAuthority after a step, as a crash would.
var authorityMigrationStep = func(int) error { return nil }

// buildAuthority writes rows into native-authority.db.tmp from scratch,
// checks it (the runtime handle's checks, integrity_check, foreign keys,
// every row decoded and the ledger validated, then check), fsyncs it and
// renames it over native-authority.db, then fsyncs the directory.
func buildAuthority(ctx context.Context, dir string, rows authorityRows, check func(lifecycleFile, *nativeLedger) error) error {
	path := filepath.Join(dir, NativeAuthorityFile)
	tmp := path + ".tmp"
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + "-journal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := writeAuthorityV3(ctx, tmp, authoritySchemaV3, rows); err != nil {
		return err
	}
	if err := verifyAuthorityFile(ctx, tmp, check); err != nil {
		return err
	}
	for _, p := range []string{tmp + "-wal", tmp + "-shm"} {
		if info, err := os.Lstat(p); err == nil && info.Size() > 0 && strings.HasSuffix(p, "-wal") {
			return ErrNativeMigration // every row must be in the file before the rename
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// A WAL or shared-memory file of an earlier database would be replayed
	// onto this one. Only offline callers (migration) or a fresh install
	// (no database yet) get here.
	dropAuthorityHandle(path)
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return fsutil.SyncDir(dir)
}

// verifyAuthorityFile opens path as the runtime does and validates all of it.
func verifyAuthorityFile(ctx context.Context, path string, check func(lifecycleFile, *nativeLedger) error) error {
	if _, err := privateRegular(path); err != nil {
		return err
	}
	db, err := openAuthorityDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	r := authorityRows{}
	for name, query := range map[string]string{"integrity": "PRAGMA integrity_check", "fk": "PRAGMA foreign_key_check"} {
		if err := queryRows(ctx, db, name, query, r); err != nil {
			return authorityErr(err)
		}
	}
	if len(r["integrity"]) != 1 || r["integrity"][0][0] != "ok" || len(r["fk"]) != 0 {
		return ErrNativeMigration
	}
	for _, table := range authorityTablesV3 {
		if err := readTable(ctx, db, table, r); err != nil {
			return authorityErr(err)
		}
	}
	life, l, err := r.decode()
	if err != nil {
		return err
	}
	if l != nil {
		if _, _, err := validateNativeLedger(*l); err != nil {
			return err
		}
	}
	if check != nil {
		return check(life, l)
	}
	return nil
}

// readAuthorityFile decodes a whole database through a verified handle.
func readAuthorityFile(ctx context.Context, path string) (lifecycleFile, *nativeLedger, error) {
	var life lifecycleFile
	var l *nativeLedger
	err := verifyAuthorityFile(ctx, path, func(gotLife lifecycleFile, gotLedger *nativeLedger) error {
		life, l = gotLife, gotLedger
		return nil
	})
	return life, l, err
}

// sameAuthority compares decoded values canonically (JSON, so an empty and a
// missing optional map are equal) and their recovery digests.
func sameAuthority(configDir string, a lifecycleFile, al *nativeLedger, b lifecycleFile, bl *nativeLedger) bool {
	a.ensureMaps()
	b.ensureMaps()
	ja, errA := json.Marshal([]any{a, al})
	jb, errB := json.Marshal([]any{b, bl})
	if errA != nil || errB != nil || string(ja) != string(jb) {
		return false
	}
	digest := func(life lifecycleFile, l *nativeLedger) string {
		f := nativeAssignments{Accounts: map[string]NativeAssignment{}}
		if l != nil {
			var err error
			if f, _, err = validateNativeLedger(*l); err != nil {
				return "invalid"
			}
		}
		c, _, _, err := recoveryInputs("migration", NewStore(configDir).Load(), make([]byte, 32), nil, life, f)
		if err != nil {
			return "refused"
		}
		return c.AuthorityDigest
	}
	return digest(a, al) == digest(b, bl)
}

// MigrateAuthority is migrate-native's v2 → v3 step (section 5), under
// domains → directory → users locks:
//  1. copy sso-lifecycle.json and native-provisioning.json to *.v2-migrated
//     while each still holds v2 data;
//  2. validate the copies (full decode, validation, seeded generations, the
//     initialization rule) before any output;
//  3. build native-authority.db by rename, after integrity, whole-store and
//     round-trip (values and recovery digest) checks;
//  4. write the lifecycle stub, 5. the ledger stub last.
//
// Every output derives from the copies and is overwritten, so a crash at any
// step converges on re-run; once both stubs are in place and the database
// validates it is a no-op. A database that differs from the copies' own
// conversion holds v3 writes, so v2 files beside it (a hand adoption of the
// copies) are refused, never adopted. It grants no authority and leaves a
// restore hold in place.
func MigrateAuthority(ctx context.Context, configDir string) (bool, error) {
	if !anyExists(configDir, []string{nativeLifecycleFile, nativeProvisioningFile, NativeAuthorityFile, nativeLifecycleFile + NativeAuthorityCopySuffix, nativeProvisioningFile + NativeAuthorityCopySuffix}) {
		return false, nil // fresh install: the first write creates v3
	}
	for _, lock := range []string{NativeDomainsFile, nativeLifecycleFile, "users.json"} {
		release, err := fsutil.LockFileContext(ctx, filepath.Join(configDir, lock))
		if err != nil {
			return false, err
		}
		defer release()
	}
	if _, data, err := legacyNativeDomain(configDir); err != nil || data {
		return false, errors.Join(err, errors.New("native-domain.json still holds version-1 data; the version-1 migration must finish first"))
	}
	lifePath, ledgerPath := filepath.Join(configDir, nativeLifecycleFile), filepath.Join(configDir, nativeProvisioningFile)
	dbPath := filepath.Join(configDir, NativeAuthorityFile)
	l, err := stubFile(lifePath, lifecycleStubV3)
	if err != nil {
		return false, fmt.Errorf("%s: %w", nativeLifecycleFile, err)
	}
	p, err := stubFile(ledgerPath, ledgerStubV3)
	if err != nil {
		return false, fmt.Errorf("%s: %w", nativeProvisioningFile, err)
	}
	_, dbErr := os.Lstat(dbPath)
	if dbErr != nil && !errors.Is(dbErr, os.ErrNotExist) {
		return false, dbErr
	}
	db := dbErr == nil
	switch {
	case db && l != fileData && p != fileData:
		if p == fileStub && l != fileStub {
			return false, errors.New("native-provisioning.json is a v3 stub but sso-lifecycle.json is not; restore a consistent backup")
		}
		// v3, or a first write that crashed before its stubs: validate, finish.
		if _, _, err := readAuthorityFile(ctx, dbPath); err != nil {
			return false, fmt.Errorf("%s: %w", NativeAuthorityFile, err)
		}
		if l == fileStub && p == fileStub {
			return false, nil
		}
		return true, writeAuthorityStubs(configDir)
	case l == fileAbsent && p == fileAbsent:
		for _, path := range []string{lifePath, ledgerPath} {
			if _, err := os.Lstat(path + NativeAuthorityCopySuffix); err == nil {
				return false, fmt.Errorf("%s is missing but %s%s is left over from an earlier migration; move that copy aside and restart", filepath.Base(path), filepath.Base(path), NativeAuthorityCopySuffix)
			}
		}
		return false, nil // fresh install: the first write creates v3
	case p == fileStub || l == fileStub && !db:
		return false, errors.New("a v3 stub without native-authority.db; restore a consistent backup")
	}
	// v2 sources remain. With a database beside them it must be exactly the
	// conversion of the existing copies (a crash after step 3), else refuse.
	if db {
		copied, copiedLedger, err := authorityCopies(configDir)
		if err != nil {
			return false, err
		}
		got, gotLedger, err := readAuthorityFile(ctx, dbPath)
		if err != nil || !sameAuthority(configDir, got, gotLedger, copied, copiedLedger) {
			return false, fmt.Errorf("native-authority.db beside version-2 files holds authority the *%s copies lack (written after migration); never adopt the copies after v3 traffic: move sso-lifecycle.json and native-provisioning.json aside if they are a hand restore, or restore a consistent backup", NativeAuthorityCopySuffix)
		}
	}
	// 1. Preserve.
	for _, src := range []struct {
		path  string
		state int
	}{{lifePath, l}, {ledgerPath, p}} {
		switch src.state {
		case fileData:
			raw, err := os.ReadFile(src.path)
			if err != nil {
				return false, err
			}
			if err := fsutil.AtomicWriteFile(src.path+NativeAuthorityCopySuffix, raw, 0600); err != nil {
				return false, err
			}
		case fileAbsent:
			if _, err := os.Lstat(src.path + NativeAuthorityCopySuffix); err == nil {
				return false, fmt.Errorf("%s is missing but %s%s is left over from an earlier migration; move that copy aside and restart", filepath.Base(src.path), filepath.Base(src.path), NativeAuthorityCopySuffix)
			}
		}
	}
	if err := authorityMigrationStep(1); err != nil {
		return false, err
	}
	// 2. Validate the copies before any output.
	life, ledger, err := authorityCopies(configDir)
	if err != nil {
		return false, err
	}
	rows, err := authorityV3Rows(life, ledger)
	if err != nil {
		return false, fmt.Errorf("native authority copies: %w", err)
	}
	if ledger != nil {
		// v2 rewrote every inactive route on each commit; v3 starts with all
		// of them pending, so the first write converges them.
		for address, x := range ledger.Addresses {
			if x.State != "active" {
				rows.add("routes_pending", address, x.Generation)
			}
		}
	}
	if err := authorityMigrationStep(2); err != nil {
		return false, err
	}
	// 3. Build the database.
	if err := buildAuthority(ctx, configDir, rows, func(got lifecycleFile, gotLedger *nativeLedger) error {
		if !sameAuthority(configDir, got, gotLedger, life, ledger) {
			return fmt.Errorf("%w: the converted database does not read back as its sources", ErrNativeMigration)
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("%s: %w", NativeAuthorityFile, err)
	}
	if err := authorityMigrationStep(3); err != nil {
		return false, err
	}
	// 4 and 5. The lifecycle stub, then the ledger stub last.
	if err := fsutil.PersistJSONFile(lifePath, lifecycleStubV3); err != nil {
		return false, err
	}
	if err := authorityMigrationStep(4); err != nil {
		return false, err
	}
	return true, fsutil.PersistJSONFile(ledgerPath, ledgerStubV3)
}

// authorityCopies decodes and validates the *.v2-migrated copies (absent
// ones are empty): the whole lifecycle, the whole v2 ledger with seeded
// generations, and the initialization rule (enforced by authorityV3Rows).
func authorityCopies(configDir string) (lifecycleFile, *nativeLedger, error) {
	life := lifecycleFile{}
	raw, err := os.ReadFile(filepath.Join(configDir, nativeLifecycleFile+NativeAuthorityCopySuffix))
	switch {
	case err == nil:
		if err := decodeLifecycle(raw, &life); err != nil {
			return life, nil, fmt.Errorf("%s%s: %w", nativeLifecycleFile, NativeAuthorityCopySuffix, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return life, nil, err
	}
	life.ensureMaps()
	raw, err = os.ReadFile(filepath.Join(configDir, nativeProvisioningFile+NativeAuthorityCopySuffix))
	if errors.Is(err, os.ErrNotExist) {
		return life, nil, nil
	}
	if err != nil {
		return life, nil, err
	}
	f, version, err := parseNativeLedger(raw, false)
	if err != nil || version != 2 {
		return life, nil, fmt.Errorf("%s%s: %w", nativeProvisioningFile, NativeAuthorityCopySuffix, cmp.Or(err, ErrNativeMigration))
	}
	l := f.stored
	l.seedGenerations(life.Directory)
	l.AddressGenerations = true
	return life, &l, nil
}
