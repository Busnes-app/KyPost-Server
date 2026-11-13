//go:build linux

package sso

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"
	"modernc.org/sqlite"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
)

// rewriteAuthorityForTest rebuilds native-authority.db from the current rows
// after edit, with schema, bypassing every trigger and validation: damage
// written outside the store. A nil ledger writes no ledger rows.
func rewriteAuthorityForTest(t *testing.T, s *LifecycleStore, schema string, edit func(*lifecycleFile, **nativeLedger)) {
	t.Helper()
	dir := filepath.Dir(s.path)
	path := filepath.Join(dir, NativeAuthorityFile)
	db, err := openAuthorityDB(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := authorityRows{}
	for _, table := range authorityTablesV3 {
		if err := readTable(context.Background(), db, table, rows); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	life, l, err := rows.decode() // rows only: the ledger may already be damaged
	if err != nil {
		t.Fatal(err)
	}
	change := int64(0)
	if err := s.authority().View(context.Background(), func(r authorityReader) error {
		change, err = r.Change()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	edit(&life, &l)
	rows = authorityRows{}
	rows.add("meta", "change", strconv.FormatInt(change, 10))
	rows.add("meta", "format", "3")
	rows.add("meta", "initialized", strconv.FormatInt(flag(life.NativeProvisioningInitialized), 10))
	if err := rows.lifecycleRows(life); err != nil {
		t.Fatal(err)
	}
	if l != nil {
		rows.ledgerRows(*l)
	}
	tmp := filepath.Join(t.TempDir(), "rewrite.db")
	if err := writeAuthorityV3(context.Background(), tmp, schema, rows); err != nil {
		t.Fatal(err)
	}
	dropAuthorityHandle(path)
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// editLedgerForTest rewrites the ledger with edit applied, triggers bypassed.
func editLedgerForTest(t *testing.T, s *LifecycleStore, edit func(*nativeLedger)) {
	t.Helper()
	rewriteAuthorityForTest(t, s, authoritySchemaV3, func(_ *lifecycleFile, l **nativeLedger) {
		if *l == nil {
			t.Fatal("no ledger to edit")
		}
		edit(*l)
	})
}

// writeV2ForTest turns a v3 config directory back into the v2 files holding
// the same values, as a v2 binary left them: the migration's input.
func writeV2ForTest(t *testing.T, config string) {
	t.Helper()
	path := filepath.Join(config, NativeAuthorityFile)
	life, l, err := readAuthorityFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	dropAuthorityHandle(path)
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Join(config, nativeProvisioningFile)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := fsutil.PersistJSONFile(filepath.Join(config, nativeLifecycleFile), life); err != nil {
		t.Fatal(err)
	}
	if l != nil {
		if err := fsutil.PersistJSONFile(filepath.Join(config, nativeProvisioningFile), *l); err != nil {
			t.Fatal(err)
		}
	}
}

// migrateForTest runs the v2 -> v3 migration and requires it to convert.
func migrateForTest(t *testing.T, config string) {
	t.Helper()
	if moved, err := MigrateAuthority(context.Background(), config); err != nil || !moved {
		t.Fatal("v3 migration", moved, err)
	}
}

// migrateNativeV3 is migrate-native's storage steps: v1 -> v2, then v2 -> v3.
func migrateNativeV3(ctx context.Context, config, keyPath string) (bool, error) {
	migrated, err := MigrateNative(ctx, config, keyPath)
	if err != nil {
		return migrated, err
	}
	moved, err := MigrateAuthority(ctx, config)
	return migrated || moved, err
}

// routesPendingForTest reads routes_pending.
func routesPendingForTest(t *testing.T, s *LifecycleStore) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := sqliteAuthority{path: filepath.Join(filepath.Dir(s.path), NativeAuthorityFile)}.View(context.Background(), func(r authorityReader) error {
		rows := authorityRows{}
		if err := queryRows(context.Background(), r.(*sqliteTx).c, "p", "SELECT address, generation FROM routes_pending", rows); err != nil {
			return err
		}
		for _, row := range rows["p"] {
			out[row[0].(string)] = row[1].(int64)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// authorityContent is every authority row and both stub files (ssotest).
func authorityContent(s *LifecycleStore) ([]byte, error) {
	return ssotest.AuthorityContent(filepath.Dir(s.path))
}

// TestAuthorityReplaceCannotBypassTriggers (findings S4, S5) on the store's
// own held handle: REPLACE into an existing row (a lower address generation,
// an account), and plain UPDATEs of history and of every immutable column,
// fail and leave every row unchanged.
func TestAuthorityReplaceCannotBypassTriggers(t *testing.T) {
	f := v3Fixture(t)
	path := filepath.Join(f.config, NativeAuthorityFile)
	h, err := acquireAuthority(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAuthority(path, h, false)
	var recursive, trusted int
	if err := h.db.QueryRow("SELECT (SELECT recursive_triggers FROM pragma_recursive_triggers), (SELECT trusted_schema FROM pragma_trusted_schema)").Scan(&recursive, &trusted); err != nil || recursive != 1 || trusted != 0 {
		t.Fatal("store handle pragmas", recursive, trusted, err)
	}
	before := mustContent(t, f.config)
	for _, stmt := range []string{
		"REPLACE INTO addresses SELECT address, mailbox, kind, state, 1 FROM addresses WHERE address = 'sales@example.test'",
		"INSERT OR REPLACE INTO addresses SELECT address, mailbox, kind, state, 1 FROM addresses WHERE address = 'sales@example.test'",
		"REPLACE INTO accounts SELECT * FROM accounts LIMIT 1",
		"UPDATE address_history SET mailbox = 'x' WHERE address = 'sales@example.test'",
		"UPDATE address_history SET generation = generation + 100 WHERE address = 'sales@example.test'",
		"UPDATE addresses SET generation = 1 WHERE address = 'sales@example.test'",
		"UPDATE addresses SET address = 'other@example.test' WHERE address = 'sales@example.test'",
		"UPDATE mailboxes SET id = 'x' WHERE kind = 'extra'", "UPDATE mailboxes SET issuer = 'x'", "UPDATE mailboxes SET subject = 'x'",
		"UPDATE mailboxes SET kind = 'extra' WHERE kind = 'primary'", "UPDATE mailboxes SET state_root = 'x'",
		"UPDATE mailboxes SET source = 'x' WHERE source <> ''",
		"UPDATE accounts SET issuer = 'x'", "UPDATE accounts SET subject = 'x'", "UPDATE accounts SET primary_mailbox = 'x'",
		"DELETE FROM accounts", "DELETE FROM mailboxes", "DELETE FROM addresses", "DELETE FROM address_history",
		"PRAGMA writable_schema = ON; UPDATE sqlite_schema SET sql = '' WHERE name = 'addresses_no_delete'",
	} {
		if _, err := h.db.Exec(stmt); err == nil {
			t.Error("allowed:", stmt)
		}
	}
	if after := mustContent(t, f.config); !bytes.Equal(before, after) {
		t.Fatal("a refused statement changed rows")
	}
}

func mustContent(t *testing.T, config string) []byte {
	t.Helper()
	raw, err := ssotest.AuthorityContent(config)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The migration killed after each step converges on re-run to the clean
// run's result; a second run is a no-op; a v2 binary writing between steps
// is picked up; a stale temp file is ignored.
func TestAuthorityMigrationConvergesAfterEachStep(t *testing.T) {
	ctx := context.Background()
	f := v3Fixture(t)
	writeV2ForTest(t, f.config)
	clean := copyConfig(t, f.config)
	migrateForTest(t, clean)
	want := mustContent(t, clean)
	crash := errors.New("simulated crash")
	stopAfter := func(k int) {
		authorityMigrationStep = func(step int) error {
			if step == k {
				return crash
			}
			return nil
		}
	}
	t.Cleanup(func() { authorityMigrationStep = func(int) error { return nil } })
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprint("step-", k), func(t *testing.T) {
			dir := copyConfig(t, f.config)
			if err := os.WriteFile(filepath.Join(dir, NativeAuthorityFile+".tmp"), []byte("stale"), 0600); err != nil {
				t.Fatal(err)
			}
			stopAfter(k)
			if _, err := MigrateAuthority(ctx, dir); !errors.Is(err, crash) {
				t.Fatal(err)
			}
			if _, err := NewLifecycleStore(dir).load(); !errors.Is(err, ErrNativeMigration) {
				t.Fatal("half-migrated storage served", err)
			}
			stopAfter(0)
			if moved, err := MigrateAuthority(ctx, dir); err != nil || !moved {
				t.Fatal(moved, err)
			}
			if got := mustContent(t, dir); !bytes.Equal(got, want) {
				t.Fatalf("resumed migration diverged from a clean run:\n%s\n%s", got, want)
			}
			before := durableFiles(t, dir)
			if moved, err := MigrateAuthority(ctx, dir); err != nil || moved {
				t.Fatal("second run was not a no-op", moved, err)
			}
			if after := durableFiles(t, dir); !maps.Equal(before, after) {
				t.Fatal("second run changed files")
			}
		})
	}
	// A v2 binary writes between the copy and the build: re-copied.
	dir := copyConfig(t, f.config)
	stopAfter(2)
	if _, err := MigrateAuthority(ctx, dir); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	stopAfter(0)
	v2 := NewLifecycleStore(dir).staged()
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, _, err := v2.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "two", JWTID: "late", IssuedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	migrateForTest(t, dir)
	if out, err := NewLifecycleStore(dir).LoggedOut(SessionIdentity{Issuer: nativeIssuer, ClientID: "client", Subject: "two", IssuedAt: at}); err != nil || !out {
		t.Fatal("v2 write between steps lost", out, err)
	}
}

// Finding S1: once v3 has taken writes, v2 files put back beside the database
// (a hand adoption of the copies) are refused and nothing is written. A copy
// left without its source refuses too.
func TestAuthorityMigrationRefusesV2Adoption(t *testing.T) {
	ctx := context.Background()
	f := v3Fixture(t)
	writeV2ForTest(t, f.config)
	migrateForTest(t, f.config)
	at := time.Now().Add(time.Hour)
	if _, _, err := f.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "after-v3", IssuedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{nativeLifecycleFile, nativeProvisioningFile} {
		if err := os.WriteFile(filepath.Join(f.config, name), mustRead(t, filepath.Join(f.config, name+NativeAuthorityCopySuffix)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := durableFiles(t, f.config)
	if _, err := MigrateAuthority(ctx, f.config); err == nil || !strings.Contains(err.Error(), "never adopt") {
		t.Fatal("v2 copies adopted after v3 writes", err)
	}
	if after := durableFiles(t, f.config); !maps.Equal(before, after) {
		t.Fatal("refused migration wrote")
	}
	orphan := t.TempDir()
	if err := os.WriteFile(filepath.Join(orphan, nativeLifecycleFile+NativeAuthorityCopySuffix), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateAuthority(ctx, orphan); err == nil {
		t.Fatal("leftover copy without its source migrated")
	}
}

// The runtime format table (section 5): a fresh install reads empty and
// writes nothing until its first write, which creates the database and both
// stubs; a first write that crashed after the database is served and its
// stubs finished by the next write.
func TestAuthorityRuntimeFormatTable(t *testing.T) {
	config := t.TempDir()
	s := NewLifecycleStore(config)
	if life, err := s.load(); err != nil || len(life.Directory) != 0 {
		t.Fatal("fresh install", err)
	}
	if entries, _ := os.ReadDir(config); len(entries) != 0 {
		t.Fatal("a read created files", entries)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	logout := func(jti string) {
		t.Helper()
		if _, fresh, err := s.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: jti, IssuedAt: at}, at); err != nil || !fresh {
			t.Fatal(jti, fresh, err)
		}
	}
	logout("first")
	if format, err := detectAuthority(config); err != nil || format != authorityV3 {
		t.Fatal("first write", format, err)
	}
	for _, name := range []string{nativeProvisioningFile, nativeLifecycleFile} {
		if err := os.Remove(filepath.Join(config, name)); err != nil {
			t.Fatal(err)
		}
		if format, err := detectAuthority(config); err != nil || format != authorityFreshCrash {
			t.Fatal("fresh-create crash", name, format, err)
		}
		if out, err := s.LoggedOut(SessionIdentity{Issuer: nativeIssuer, ClientID: "client", Subject: "one", IssuedAt: at}); err != nil || !out {
			t.Fatal("fresh-create crash not served", out, err)
		}
	}
	logout("second")
	if format, err := detectAuthority(config); err != nil || format != authorityV3 {
		t.Fatal("the writer did not finish the stubs", format, err)
	}
	for _, name := range []string{NativeAuthorityFile, NativeAuthorityFile + "-wal", NativeAuthorityFile + "-shm"} {
		if info, err := os.Lstat(filepath.Join(config, name)); err == nil && info.Mode().Perm()&0077 != 0 {
			t.Fatal("not private", name, info.Mode())
		}
	}
}

// The held handle reads current committed rows: changes committed in place by
// another process are seen by the next admission, and a database replaced
// under the handle is refused inside the View that saw it, then reopened.
func TestAuthorityHeldHandleSeesOtherProcess(t *testing.T) {
	for _, change := range []string{"offboard", "source", "role"} {
		t.Run(change, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			id := f.ids["one"]
			if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestAuthorityWriterProcess$")
			command.Env = append(os.Environ(), "KYPOST_AUTHORITY_CHILD="+change, "KYPOST_AUTHORITY_CONFIG="+f.config)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("independent writer failed: %v\n%s", err, output)
			}
			if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err == nil {
				t.Fatal("held handle missed another process's committed change")
			}
		})
	}
	t.Run("replaced", func(t *testing.T) {
		f := newAddressFixture(t, "one")
		path := filepath.Join(f.config, NativeAuthorityFile)
		dropAuthorityHandle(path) // closing checkpoints, so the file alone holds every row
		err := f.life.authority().View(context.Background(), func(r authorityReader) error {
			copied := path + ".copy"
			if err := os.WriteFile(copied, mustRead(t, path), 0600); err != nil {
				return err
			}
			return os.Rename(copied, path)
		})
		if !errors.Is(err, ErrNativeMigration) {
			t.Fatal("rows from a replaced file served", err)
		}
		id := f.ids["one"]
		if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err != nil {
			t.Fatal("the replacement was not reopened", err)
		}
	})
}

func TestAuthorityWriterProcess(t *testing.T) {
	change := os.Getenv("KYPOST_AUTHORITY_CHILD")
	if change == "" {
		return
	}
	config := os.Getenv("KYPOST_AUTHORITY_CONFIG")
	s := NewLifecycleStore(config)
	switch change {
	case "offboard":
		nativeDesired(t, s, "one", "one@example.test", 2, false)
	case "source":
		if err := ssotest.DamageAuthority(config, "UPDATE mailboxes SET source = 'foreign-source' WHERE kind = 'primary'"); err != nil {
			t.Fatal(err)
		}
	case "role":
		release, err := s.LockDirectory()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		f, err := s.load()
		if err != nil {
			t.Fatal(err)
		}
		f.Directory[directoryKey(nativeIssuer, "one")].Resource.Roles = json.RawMessage(`["kypost.admin"]`)
		if err := s.persistLifecycle(f); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown test writer")
	}
}

// meta.change moves with every mailbox or address commit (finding H6, the
// Cloudflare republication mark), and not with lifecycle-only writes.
func TestAuthorityChangeCounter(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	mark := func() int64 {
		t.Helper()
		n, err := f.life.NativeChange()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := mark()
	at := time.Now().Add(time.Hour)
	if _, _, err := f.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "j", IssuedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	if mark() != start {
		t.Fatal("a logout moved the routing mark")
	}
	if _, err := f.life.AddNativeAlias(ctx, f.root, f.ids["one"], "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if mark() != start+1 {
		t.Fatal("an address change did not move the routing mark", mark(), start)
	}
}

// durableFiles is configFiles without SQLite's -shm index (readers rewrite
// it) or an empty WAL (readers create and remove it).
func durableFiles(t *testing.T, config string) map[string]string {
	t.Helper()
	files := configFiles(t, config, "")
	maps.DeleteFunc(files, func(name, raw string) bool {
		return strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-wal") && raw == ""
	})
	return files
}

// Every pooled connection is verified when it opens: a DSN missing any
// required pragma is refused by the connector itself (the runtime DSN always
// sets them, so only this test reaches the checks).
func TestAuthorityConnectionChecks(t *testing.T) {
	f := newAddressFixture(t, "one")
	path, err := filepath.Abs(filepath.Join(f.config, NativeAuthorityFile))
	if err != nil {
		t.Fatal(err)
	}
	base := (&url.URL{Scheme: "file", Path: path}).String()
	dropAuthorityHandle(path) // a journal mode change needs the file to itself
	connect := func(dsn string) error {
		c, err := sqlite.NewConnector(base + dsn)
		if err != nil {
			return err
		}
		conn, err := verifiedConnector{c}.Connect(context.Background())
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	if err := connect(authorityDSN); err != nil {
		t.Fatal("runtime DSN refused", err)
	}
	for name, drop := range map[string][2]string{
		"trusted_schema":     {"&_pragma=trusted_schema(0)", ""},
		"recursive_triggers": {"&_pragma=recursive_triggers(1)", ""},
		"foreign_keys":       {"&_pragma=foreign_keys(1)", ""},
		"synchronous":        {"&_pragma=synchronous(FULL)", "&_pragma=synchronous(NORMAL)"},
		"defensive":          {"&_defensive=1", ""},
		"journal_mode":       {"&_pragma=journal_mode(WAL)", "&_pragma=journal_mode(DELETE)"},
	} {
		dsn := strings.Replace(authorityDSN, drop[0], drop[1], 1)
		if dsn == authorityDSN {
			t.Fatal("mutation did not apply", name)
		}
		if err := connect(dsn); !errors.Is(err, ErrNativeMigration) {
			t.Error("connection without", name, "accepted", err)
		}
	}
}

// Damage outside the store that the per-read rules catch.
func TestAuthorityRowDamageRefuses(t *testing.T) {
	ctx := context.Background()
	for name, damage := range map[string]string{
		"format":           "UPDATE meta SET value = '2' WHERE key = 'format'",
		"primary kind":     "UPDATE mailboxes SET kind = 'extra' WHERE kind = 'primary'",
		"extra no primary": "UPDATE addresses SET kind = 'alias' WHERE address = 'box@example.test'",
		"history order":    "UPDATE address_history SET generation = 9 WHERE address = 'one@example.test'",
	} {
		t.Run(name, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			box, err := f.life.CreateNativeMailbox(ctx, f.root, f.ids["one"], "box@example.test")
			if err != nil {
				t.Fatal(err)
			}
			if err := ssotest.DamageAuthority(f.config, damage); err != nil {
				t.Fatal(err)
			}
			_, _, err = f.life.NativeAssignment(nativeIssuer, "one")
			_, _, boxErr := f.life.NativeMailboxAssignment(box.ID)
			_, addrErr := f.life.NativeAddress("one@example.test")
			if err == nil && boxErr == nil && addrErr == nil {
				t.Fatal("damaged rows read")
			}
		})
	}
}

// Writer rules on the SQLite transaction itself.
func TestAuthorityWriterRules(t *testing.T) {
	f := newAddressFixture(t, "one")
	before := mustContent(t, f.config)
	// Even an unchanged, valid record: every ledger write starts with SetInitialized.
	if err := f.life.update(func(w authorityWriter) error {
		ledger, _, err := w.Snapshot()
		if err != nil {
			return err
		}
		key := directoryKey(nativeIssuer, "one")
		return w.PutAccount(key, ledger.stored.Accounts[key])
	}); err == nil {
		t.Fatal("ledger put without SetInitialized")
	}
	// A record the whole-ledger rules refuse (no history) is never committed.
	if err := f.life.update(func(w authorityWriter) error {
		if err := w.SetInitialized(); err != nil {
			return err
		}
		return w.PutAddress("loose@example.test", nativeLedgerAddress{Mailbox: f.ids["one"], Kind: "alias", State: "disabled", Generation: 1})
	}); err == nil {
		t.Fatal("invalid ledger committed")
	}
	if !bytes.Equal(before, mustContent(t, f.config)) {
		t.Fatal("refused writes changed rows")
	}
}

// A corrupt page that no read touches is caught by the whole-store checks:
// the minute pass (quick_check, refusing its writes) and migrate-native.
func TestAuthorityWholeStoreChecks(t *testing.T) {
	f := newAddressFixture(t, "one")
	path := filepath.Join(f.config, NativeAuthorityFile)
	dropAuthorityHandle(path) // checkpoint: every page in the file
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var page, size int64
	err = db.QueryRow("SELECT rootpage, (SELECT page_size FROM pragma_page_size) FROM sqlite_schema WHERE name = 'logouts_expiry'").Scan(&page, &size)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0x7f, 0x7f, 0x7f, 0x7f}, (page-1)*size); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := f.life.ReconcileNativeAddresses(context.Background()); err == nil {
		t.Fatal("minute pass wrote over a corrupt store")
	}
	if _, err := MigrateAuthority(context.Background(), f.config); err == nil {
		t.Fatal("migrate-native accepted a corrupt store")
	}
}

// The immutable historical opener refuses a database whose WAL holds rows.
func TestAuthorityHistoricalOpenerRefusesWAL(t *testing.T) {
	f := newAddressFixture(t, "one")
	path := filepath.Join(f.config, NativeAuthorityFile)
	if wal, err := os.Stat(path + "-wal"); err != nil || wal.Size() == 0 {
		t.Fatal("fixture has no WAL", err)
	}
	if _, err := readAuthorityV3Historical(context.Background(), path); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("rows in a WAL ignored", err)
	}
}

// A WAL left by an earlier database is never replayed onto a new one.
func TestAuthorityFreshCreateIgnoresStaleWAL(t *testing.T) {
	old := newAddressFixture(t, "one")
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, _, err := old.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "old", IssuedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	wal := mustRead(t, filepath.Join(old.config, NativeAuthorityFile+"-wal"))
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, NativeAuthorityFile+"-wal"), wal, 0600); err != nil {
		t.Fatal(err)
	}
	s := NewLifecycleStore(config)
	if _, err := s.RecordSignOnJTI(nativeIssuer, "client", "new", at); err != nil {
		t.Fatal(err)
	}
	if out, err := s.LoggedOut(SessionIdentity{Issuer: nativeIssuer, ClientID: "client", Subject: "one", IssuedAt: at}); err != nil || out {
		t.Fatal("stale WAL replayed into the new database", out, err)
	}
	if err := s.authority().View(context.Background(), func(r authorityReader) error { return r.Check() }); err != nil {
		t.Fatal(err)
	}
}

// Without a domain tombstone, any mix outside the format table refuses.
func TestAuthorityRuntimeRefusesMixes(t *testing.T) {
	at := time.Now().Add(time.Hour)
	for name, edit := range map[string]func(config string) error{
		"v2 ledger beside the database": func(c string) error {
			return os.WriteFile(filepath.Join(c, nativeProvisioningFile), []byte(`{"version":2}`), 0600)
		},
		"v2 lifecycle beside the database": func(c string) error {
			return os.WriteFile(filepath.Join(c, nativeLifecycleFile), []byte(`{}`), 0600)
		},
		"stubs without the database": func(c string) error {
			dropAuthorityHandle(filepath.Join(c, NativeAuthorityFile))
			return os.Remove(filepath.Join(c, NativeAuthorityFile))
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := t.TempDir()
			s := NewLifecycleStore(config)
			if _, _, err := s.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", JWTID: "j", IssuedAt: at}, at); err != nil {
				t.Fatal(err)
			}
			if err := edit(config); err != nil {
				t.Fatal(err)
			}
			if _, err := s.load(); !errors.Is(err, ErrNativeMigration) {
				t.Fatal("mixed format served", err)
			}
		})
	}
}

// Each connection prepares a query once: a nested run of the same query
// prepares its own statement, the cache stays bounded and the statements
// are finalized with the connection.
func TestAuthorityStatementCache(t *testing.T) {
	f := newAddressFixture(t, "one")
	db, err := openAuthorityDB(filepath.Join(f.config, NativeAuthorityFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const query = "SELECT value FROM meta WHERE key = ?"
	r := authorityRows{}
	if err := queryRows(ctx, c, "warm", query, r, "format"); err != nil {
		t.Fatal(err)
	}
	outer, err := c.QueryContext(ctx, query, "format")
	if err != nil {
		t.Fatal(err)
	}
	if err := queryRows(ctx, c, "inner", query, r, "initialized"); err != nil || len(r["inner"]) != 1 || r["inner"][0][0] != "1" {
		t.Fatal("nested run of a cached query", r, err)
	}
	var conn *stmtConn
	if err := c.Raw(func(dc any) error { conn = dc.(*stmtConn); return nil }); err != nil {
		t.Fatal(err)
	}
	inner := conn.stmts[query]
	var format string
	if !outer.Next() || outer.Scan(&format) != nil || format != "3" || outer.Next() || outer.Close() != nil {
		t.Fatal("outer rows disturbed by the nested run", format)
	}
	for i := range maxConnStmts + 8 {
		if err := queryRows(ctx, c, "n", fmt.Sprintf("SELECT %d", i), r); err != nil {
			t.Fatal(err)
		}
	}
	if conn.stmts[query] != inner || len(conn.stmts) != maxConnStmts {
		t.Fatal("cache size or a second statement replaced the cached one", len(conn.stmts))
	}
	_ = c.Close()
	if err := db.Close(); err != nil || conn.stmts != nil {
		t.Fatal("statements outlived their connection", err)
	}
}

// viewSubject's single statement reads what View's separate queries read:
// for its subject, another subject, a missing one, and damaged meta.
func TestAuthoritySubjectView(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	a := storeAuthority{f.life}
	type reads struct {
		Dir       DirectoryState
		DirOK     bool
		Account   NativeAssignment
		AccountOK bool
		Init      bool
		Change    int64
	}
	read := func(view func(func(authorityReader) error) error, subject string) reads {
		t.Helper()
		var got reads
		if err := view(func(r authorityReader) (err error) {
			if got.Dir, got.DirOK, err = r.Directory(nativeIssuer, subject); err != nil {
				return err
			}
			if got.Account, got.AccountOK, err = r.Account(nativeIssuer, subject); err != nil {
				return err
			}
			got.Init, _ = r.Initialized()
			got.Change, err = r.Change()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	plain := func(fn func(authorityReader) error) error { return a.View(ctx, fn) }
	if r := read(plain, "one"); !r.DirOK || !r.AccountOK || !r.Init {
		t.Fatal("fixture has no account one", r)
	}
	for _, pre := range []string{"one", "other"} {
		for _, subject := range []string{"one", "absent"} {
			want := read(plain, subject)
			got := read(func(fn func(authorityReader) error) error { return a.viewSubject(ctx, nativeIssuer, pre, fn) }, subject)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("subject view of %q reading %q:\n got %+v\nwant %+v", pre, subject, got, want)
			}
		}
	}
	if err := ssotest.DamageAuthority(f.config, "UPDATE meta SET value = '2' WHERE key = 'format'"); err != nil {
		t.Fatal(err)
	}
	if err := a.viewSubject(ctx, nativeIssuer, "one", func(authorityReader) error { return nil }); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("damaged format read", err)
	}
}
