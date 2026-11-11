//go:build linux

package sso

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/oidcverify"
)

// v3Fixture is a v2 install with every ledger shape: two primaries, an alias
// moved between them (history of three), a disabled extra mailbox, and a
// logout, sign-on and release floor in the lifecycle.
func v3Fixture(t *testing.T) addressFixture {
	t.Helper()
	ctx := context.Background()
	f := newAddressFixture(t, "one", "two")
	one, two := f.ids["one"], f.ids["two"]
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.life.AddNativeAlias(ctx, f.root, one, "sales@example.test"))
	must(f.life.ReleaseNativeAlias(ctx, f.root, "sales@example.test"))
	must(f.life.ReassignNativeAddress(ctx, f.root, "sales@example.test", two))
	box, err := f.life.CreateNativeMailbox(ctx, f.root, one, "box@example.test")
	must(box, err)
	must(f.life.SetNativeMailboxState(ctx, f.root, box.ID, false))
	at := time.Now().Add(time.Hour)
	_, _, err = f.life.RecordLogout("client", oidcverify.LogoutClaims{Issuer: nativeIssuer, Subject: "one", SessionID: "sid", JWTID: "jti", IssuedAt: at}, at)
	must(nil, err)
	must(f.life.RecordSignOnJTI(nativeIssuer, "client", "sign-on", at))
	if err := f.life.RecordNativeReleaseFloors(nativeIssuer, map[string]NativeReleaseFloor{"two": {Revision: 1}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func historical(t *testing.T, config string) (lifecycleFile, nativeAssignments, int) {
	t.Helper()
	life, f, version, err := NewLifecycleStore(config).historicalAuthority()
	if err != nil {
		t.Fatal(err)
	}
	return life, f, version
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestAuthorityV3RoundTrip converts a v2 install and reads it back through
// the historical opener: the lifecycle, the ledger and the recovery digest
// are unchanged, and the stubs make the runtime (JSON backend) refuse.
func TestAuthorityV3RoundTrip(t *testing.T) {
	f := v3Fixture(t)
	settings := SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost"}
	key := []byte(strings.Repeat("k", 32))
	accounts, err := f.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	digest := func(life lifecycleFile, ledger nativeAssignments) string {
		t.Helper()
		c, _, _, err := recoveryInputs("epoch", settings, key, accounts, life, ledger)
		if err != nil {
			t.Fatal(err)
		}
		return c.AuthorityDigest
	}
	life2, ledger2, version := historical(t, f.config)
	if version != 2 || len(ledger2.Accounts) != 2 || len(ledger2.stored.Mailboxes) != 3 || len(ledger2.stored.Addresses["sales@example.test"].History) != 2 {
		t.Fatal("fixture lacks a shape", version, ledger2)
	}
	// Lifecycle records with no runtime writer here round-trip too.
	life := life2.clone()
	life.RecoveryFloors = map[string]int64{directoryKey(nativeIssuer, "one"): 7}
	life.RecoveryRepairBarriers = map[string]nativeRecoveryRepairBarrier{directoryKey(nativeIssuer, "one"): {Version: 1, Epoch: "e", Subject: "one"}}
	life.RecoveryRepair = &nativeRecoveryRepair{Issuer: nativeIssuer, Epoch: "e"}
	life.RecoveryChallenge = &NativeRecoveryChallenge{Epoch: "e", Subjects: []string{"one"}}
	life.RecoveryReceipt = &nativeRecoveryReceipt{Body: []byte("body")}
	life.RestoreRelease = &NativeRestoreReleaseRecord{Epoch: "e"}
	rows, err := authorityV3Rows(life, &ledger2.stored)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), NativeAuthorityFile)
	if err := writeAuthorityV3(context.Background(), path, authoritySchemaV3, rows); err != nil {
		t.Fatal(err)
	}
	if rows, err = readAuthorityV3Historical(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	readBack, _, err := rows.decode()
	if err != nil || canonical(t, readBack) != canonical(t, life) {
		t.Fatal("lifecycle records did not round-trip", err)
	}

	if err := WriteNativeAuthorityV3ForTest(f.config); err != nil {
		t.Fatal(err)
	}
	life3, ledger3, version := historical(t, f.config)
	if version != 3 || canonical(t, life3) != canonical(t, life2) || canonical(t, ledger3.stored) != canonical(t, ledger2.stored) || !reflect.DeepEqual(ledger3.Accounts, ledger2.Accounts) {
		t.Fatalf("v3 read differs from v2:\n%s\n%s", canonical(t, ledger3.stored), canonical(t, ledger2.stored))
	}
	if digest(life3, ledger3) != digest(life2, ledger2) {
		t.Fatal("recovery digest changed across conversion")
	}
	if _, err := f.life.load(); err == nil {
		t.Fatal("runtime read the lifecycle stub")
	}
	if err := checkNativeFormat(f.config); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("runtime accepted the ledger stub", err)
	}
	if _, err := f.life.ValidateNativeSnapshot(f.root, accounts); err != nil {
		t.Fatal("v3 snapshot refused", err)
	}
	// v3 is written only after the v1 → v2 step, so a v1 domain never sits beside it.
	if NativeSnapshotFormatsConsistent(1, 3) || !NativeSnapshotFormatsConsistent(2, 3) {
		t.Fatal("v3 format-mix rule")
	}
}

// v3Config writes a v3 config directory from the fixture's rows with schema.
func v3Config(t *testing.T, rows authorityRows, schema string) string {
	t.Helper()
	config := t.TempDir()
	if err := writeAuthorityV3(context.Background(), filepath.Join(config, NativeAuthorityFile), schema, rows); err != nil {
		t.Fatal(err)
	}
	for path, stub := range map[string]map[string]any{filepath.Join(config, "sso-lifecycle.json"): lifecycleStubV3, filepath.Join(config, nativeProvisioningFile): ledgerStubV3} {
		raw, _ := json.Marshal(stub)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return config
}

func fixtureRows(t *testing.T) authorityRows {
	t.Helper()
	life, ledger, _ := historical(t, v3Fixture(t).config)
	rows, err := authorityV3Rows(life, &ledger.stored)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestAuthoritySchemaMismatchRefuses is the historical-opener half of the
// design's test: the right tables and user_version but a dropped key, index or
// any trigger, an extra object, a wrong version or a corrupt page is refused.
func TestAuthoritySchemaMismatchRefuses(t *testing.T) {
	rows := fixtureRows(t)
	refuses := func(t *testing.T, config string) {
		t.Helper()
		if _, _, _, err := NewLifecycleStore(config).historicalAuthority(); !errors.Is(err, ErrNativeMigration) {
			t.Fatal("accepted", err)
		}
	}
	// Whitespace is normalised: a reformatted but identical schema reads.
	if _, _, v, err := NewLifecycleStore(v3Config(t, rows, strings.ReplaceAll(authoritySchemaV3, "  ", "\t "))).historicalAuthority(); err != nil || v != 3 {
		t.Fatal("reformatted schema refused", v, err)
	}
	cases := map[string]string{
		"mailbox primary key":    strings.Replace(authoritySchemaV3, "id TEXT PRIMARY KEY,", "id TEXT NOT NULL UNIQUE,", 1),
		"primary mailbox UNIQUE": strings.Replace(authoritySchemaV3, "primary_mailbox TEXT NOT NULL UNIQUE", "primary_mailbox TEXT NOT NULL", 1),
		"STRICT":                 strings.Replace(authoritySchemaV3, "CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;", "CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);", 1),
		"extra view":             authoritySchemaV3 + "CREATE VIEW extra AS SELECT 1;",
		"extra table":            authoritySchemaV3 + "CREATE TABLE extra(x);",
		"extra function view":    authoritySchemaV3 + "CREATE VIEW v AS SELECT load_extension('x');",
		"extra expression index": authoritySchemaV3 + "CREATE INDEX e ON meta(length(value));",
		"user_version":           strings.Replace(authoritySchemaV3, "user_version = 3", "user_version = 4", 1),
	}
	// Every index and every trigger, one at a time.
	for _, stmt := range regexp.MustCompile(`(?s)CREATE (?:UNIQUE INDEX|INDEX|TRIGGER) (\w+).*?;(?: END;)?\n`).FindAllStringSubmatch(authoritySchemaV3, -1) {
		cases["drop "+stmt[1]] = strings.Replace(authoritySchemaV3, stmt[0], "", 1)
	}
	if len(cases) != 8+6+11 {
		t.Fatal("schema statements not found", len(cases))
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			if schema == authoritySchemaV3 {
				t.Fatal("mutation did not apply")
			}
			refuses(t, v3Config(t, rows, schema))
		})
	}
	t.Run("corrupt page", func(t *testing.T) {
		config := v3Config(t, rows, authoritySchemaV3)
		path := filepath.Join(config, NativeAuthorityFile)
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro&immutable=1")
		if err != nil {
			t.Fatal(err)
		}
		var page, size int64
		err = db.QueryRow("SELECT rootpage, (SELECT page_size FROM pragma_page_size) FROM sqlite_schema WHERE name = 'addresses_mailbox'").Scan(&page, &size)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		// An invalid page type in an index no read uses: only integrity_check sees it.
		if _, err := file.WriteAt([]byte{0x7f, 0x7f, 0x7f, 0x7f}, (page-1)*size); err != nil {
			t.Fatal(err)
		}
		file.Close()
		refuses(t, config)
	})
	t.Run("file mode and type", func(t *testing.T) {
		config := v3Config(t, rows, authoritySchemaV3)
		path := filepath.Join(config, NativeAuthorityFile)
		if err := os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
		refuses(t, config)
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".real", path); err != nil {
			t.Fatal(err)
		}
		refuses(t, config)
	})
}

// TestAuthorityV3MixedFormatsRefuse: the database needs both exact stubs, and
// a stub never stands without the database.
func TestAuthorityV3MixedFormatsRefuse(t *testing.T) {
	f := v3Fixture(t)
	accounts, err := f.accounts.List()
	if err != nil {
		t.Fatal(err)
	}
	v2 := map[string][]byte{}
	for _, name := range []string{"sso-lifecycle.json", nativeProvisioningFile} {
		v2[name] = mustRead(t, filepath.Join(f.config, name))
	}
	if err := WriteNativeAuthorityV3ForTest(f.config); err != nil {
		t.Fatal(err)
	}
	stubs := map[string][]byte{}
	for _, name := range []string{"sso-lifecycle.json", nativeProvisioningFile} {
		stubs[name] = mustRead(t, filepath.Join(f.config, name))
	}
	db := mustRead(t, filepath.Join(f.config, NativeAuthorityFile))
	install := func(files map[string][]byte) string {
		config := t.TempDir()
		for name, raw := range files {
			if err := os.WriteFile(filepath.Join(config, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		// The domain set, so only the authority format differs.
		for _, name := range []string{NativeDomainsFile, legacyNativeDomainFile} {
			if raw, err := os.ReadFile(filepath.Join(f.config, name)); err == nil {
				if err := os.WriteFile(filepath.Join(config, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return config
	}
	l, p := "sso-lifecycle.json", nativeProvisioningFile
	for name, files := range map[string]map[string][]byte{
		"v2 lifecycle beside the database":  {l: v2[l], p: stubs[p], NativeAuthorityFile: db},
		"v2 ledger beside the database":     {l: stubs[l], p: v2[p], NativeAuthorityFile: db},
		"database without stubs":            {NativeAuthorityFile: db},
		"database without the ledger stub":  {l: stubs[l], NativeAuthorityFile: db},
		"stub with an extra field":          {l: stubs[l], p: []byte(`{"version":3,"authority":"native-authority.db","x":1}`), NativeAuthorityFile: db},
		"both stubs without the database":   {l: stubs[l], p: stubs[p]},
		"lifecycle stub beside a v2 ledger": {l: stubs[l], p: v2[p]},
		"ledger stub beside a v2 lifecycle": {l: v2[l], p: stubs[p]},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewLifecycleStore(install(files)).ValidateNativeSnapshot(f.root, accounts); err == nil {
				t.Fatal("mixed format accepted")
			}
		})
	}
	if _, err := NewLifecycleStore(install(map[string][]byte{l: stubs[l], p: stubs[p], NativeAuthorityFile: db})).ValidateNativeSnapshot(f.root, accounts); err != nil {
		t.Fatal("complete v3 refused", err)
	}
}

// TestAuthorityV3RowsRefused: the converter's preconditions and the reader's
// row rules each refuse.
func TestAuthorityV3RowsRefused(t *testing.T) {
	life, ledger, _ := historical(t, v3Fixture(t).config)
	unseeded := ledger.stored
	unseeded.AddressGenerations = false
	uninitialized := life.clone()
	uninitialized.NativeProvisioningInitialized = false
	for name, err := range map[string]error{
		"unseeded generations":        func() error { _, err := authorityV3Rows(life, &unseeded); return err }(),
		"ledger behind an unset flag": func() error { _, err := authorityV3Rows(uninitialized, &ledger.stored); return err }(),
		"invalid ledger": func() error {
			bad := ledger.stored
			bad.Addresses = maps.Clone(bad.Addresses)
			x := bad.Addresses["sales@example.test"]
			x.Mailbox = "no-such-mailbox"
			bad.Addresses["sales@example.test"] = x
			_, err := authorityV3Rows(life, &bad)
			return err
		}(),
		"set flag without a ledger": func() error { _, err := authorityV3Rows(life, nil); return err }(),
	} {
		if err == nil {
			t.Error(name, "converted")
		}
	}
	if rows, err := authorityV3Rows(uninitialized, nil); err != nil || len(rows["accounts"]) != 0 {
		t.Fatal("SSO-only lifecycle", err)
	}
	edit := func(table string, edit func([][]any) [][]any) authorityRows {
		rows, err := authorityV3Rows(life, &ledger.stored)
		if err != nil {
			t.Fatal(err)
		}
		rows[table] = edit(rows[table])
		return rows
	}
	set := func(column int, v any) func([][]any) [][]any {
		return func(rows [][]any) [][]any { rows[0][column] = v; return rows }
	}
	for name, rows := range map[string]authorityRows{
		"format":            edit("meta", func(rows [][]any) [][]any { return append(rows[:1], []any{"format", "2"}, rows[2]) }),
		"extra meta":        edit("meta", func(rows [][]any) [][]any { return append(rows, []any{"other", "x"}) }),
		"stale alias index": edit("directory_primary_emails", func(rows [][]any) [][]any { return rows[1:] }),
		"event expiry":      edit("directory_events", set(2, int64(1))),
		"logout expiry":     edit("logouts", set(2, int64(1))),
		"uninitialized":     edit("meta", func(rows [][]any) [][]any { return append(rows[:2], []any{"initialized", "0"}) }),
		"history": edit("address_history", func(rows [][]any) [][]any {
			return slices.DeleteFunc(rows, func(row []any) bool { return row[0] == "one@example.test" })
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := NewLifecycleStore(v3Config(t, rows, authoritySchemaV3)).historicalAuthority(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A row written with foreign_keys off (orphan history, an address of no
// mailbox) passes integrity_check but not the opener.
func TestAuthorityV3ForeignKeysChecked(t *testing.T) {
	for name, stmt := range map[string]string{
		"orphan history":   "INSERT INTO address_history VALUES('ghost@example.test', 1, 'x')",
		"orphan address":   "INSERT INTO addresses VALUES('ghost@example.test', 'no-such-mailbox', 'alias', 'active', 1)",
		"mailbox no owner": "INSERT INTO mailboxes VALUES('mbx-ghost', 'i', 's', 'extra', 'active', '/r', 1, 1, 1, '')",
	} {
		t.Run(name, func(t *testing.T) {
			config := v3Config(t, fixtureRows(t), authoritySchemaV3)
			path := filepath.Join(config, NativeAuthorityFile)
			db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_pragma=foreign_keys(0)")
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(stmt)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := NewLifecycleStore(config).historicalAuthority(); !errors.Is(err, ErrNativeMigration) {
				t.Fatal("foreign key violation accepted", err)
			}
		})
	}
}

// TestAuthorityV3Triggers checks that the schema's triggers hold for a
// writer handle (recursive_triggers on), REPLACE included. PR 3's runtime
// test extends this to store-opened handles.
func TestAuthorityV3Triggers(t *testing.T) {
	path := filepath.Join(v3Config(t, fixtureRows(t), authoritySchemaV3), NativeAuthorityFile)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"DELETE FROM accounts", "DELETE FROM mailboxes", "DELETE FROM addresses", "DELETE FROM address_history",
		"UPDATE address_history SET mailbox = 'x'", "UPDATE address_history SET generation = generation + 100",
		"INSERT INTO address_history VALUES('sales@example.test', 2, 'x')",
		"UPDATE addresses SET generation = 1 WHERE address = 'sales@example.test'",
		"UPDATE addresses SET address = 'other@example.test' WHERE address = 'sales@example.test'",
		"REPLACE INTO addresses SELECT address, mailbox, kind, state, 1 FROM addresses WHERE address = 'sales@example.test'",
		"REPLACE INTO accounts SELECT * FROM accounts LIMIT 1",
		"UPDATE mailboxes SET id = 'x' WHERE kind = 'extra'", "UPDATE mailboxes SET issuer = 'x'", "UPDATE mailboxes SET subject = 'x'",
		"UPDATE mailboxes SET kind = 'extra' WHERE kind = 'primary'", "UPDATE mailboxes SET state_root = 'x'",
		"UPDATE mailboxes SET source = 'x' WHERE source <> ''",
		"UPDATE accounts SET issuer = 'x'", "UPDATE accounts SET subject = 'x' WHERE subject = 'one'", "UPDATE accounts SET primary_mailbox = 'x'",
		"INSERT INTO accounts SELECT issuer, subject || '2', revision, digest, desired_active, status, failure, 'nope', 0 FROM accounts LIMIT 1",
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Error("allowed:", stmt)
		}
	}
	// The writable columns stay writable.
	for _, stmt := range []string{"UPDATE mailboxes SET state = 'disabled', records = records + 1", "UPDATE addresses SET generation = generation + 1", "UPDATE accounts SET status = 'failed', revision = revision + 1"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Error(stmt, err)
		}
	}
}

// foldKey matches strings.EqualFold after TrimSpace, as the alias rule does.
func TestFoldKeyMatchesEqualFold(t *testing.T) {
	values := []string{"a@b.test", "A@B.TEST", " a@b.test ", "k@x", "K@x", "K@X", "ß@x", "ẞ@x", "ss@x", "\xff@x", "\xfe@x", "σ@x", "ς@x", "Σ@x"}
	for _, a := range values {
		for _, b := range values {
			if got, want := foldKey(a) == foldKey(b), strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)); got != want {
				t.Errorf("%q %q: foldKey %v EqualFold %v", a, b, got, want)
			}
		}
	}
}
