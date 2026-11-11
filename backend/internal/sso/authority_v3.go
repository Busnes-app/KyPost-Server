package sso

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// NativeAuthorityFile is the v3 authority database (format D in
// docs/NATIVE_AUTHORITY_PER_ACCOUNT.md). Until the SQLite runtime backend
// lands, no binary writes it: only historical snapshots are read from it.
const NativeAuthorityFile = "native-authority.db"

// The v3 stubs left in place of the two JSON files, so older binaries refuse.
var (
	ledgerStubV3    = map[string]any{"version": 3.0, "authority": NativeAuthorityFile}
	lifecycleStubV3 = map[string]any{"movedTo": NativeAuthorityFile, "directory": NativeAuthorityFile, "logouts": "moved"}
)

// authoritySchemaV3 is the whole v3 schema. Opening requires sqlite_schema to
// match it exactly (finding S3): the keys, indexes and triggers carry the
// global invariants, and Snapshot's maps would silently collapse duplicates.
const authoritySchemaV3 = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
CREATE TABLE directory(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision >= 0), digest TEXT NOT NULL,
  active INTEGER NOT NULL CHECK(active IN (0,1)), event_id TEXT NOT NULL,
  revoked_before INTEGER NOT NULL DEFAULT 0,
  resource TEXT,
  PRIMARY KEY(issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE directory_primary_emails(
  fold TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL,
  PRIMARY KEY(fold, issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE accounts(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision INTEGER NOT NULL, digest TEXT NOT NULL, desired_active INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','applied','failed')), failure TEXT NOT NULL DEFAULT '',
  primary_mailbox TEXT NOT NULL UNIQUE,
  legacy_mixed_use INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE mailboxes(
  id TEXT PRIMARY KEY,
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('primary','extra')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled')),
  state_root TEXT NOT NULL, message_bytes INTEGER NOT NULL, payload_bytes INTEGER NOT NULL,
  records INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(issuer, subject) REFERENCES accounts DEFERRABLE INITIALLY DEFERRED) STRICT;
CREATE INDEX mailboxes_owner ON mailboxes(issuer, subject);
CREATE TABLE addresses(
  address TEXT PRIMARY KEY CHECK(address = lower(address)),
  mailbox TEXT NOT NULL REFERENCES mailboxes(id),
  kind TEXT NOT NULL CHECK(kind IN ('primary','alias')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled','reserved') AND (kind = 'alias' OR state <> 'reserved')),
  generation INTEGER NOT NULL CHECK(generation >= 1)) STRICT, WITHOUT ROWID;
CREATE UNIQUE INDEX addresses_one_primary ON addresses(mailbox) WHERE kind = 'primary';
CREATE INDEX addresses_mailbox ON addresses(mailbox);
CREATE TABLE address_history(
  address TEXT NOT NULL REFERENCES addresses, generation INTEGER NOT NULL CHECK(generation >= 1),
  mailbox TEXT NOT NULL, PRIMARY KEY(address, generation)) STRICT, WITHOUT ROWID;
CREATE TABLE routes_pending(address TEXT PRIMARY KEY, generation INTEGER NOT NULL) STRICT;
CREATE TABLE directory_events(key TEXT PRIMARY KEY, value TEXT NOT NULL, expires INTEGER NOT NULL) STRICT, WITHOUT ROWID;
CREATE INDEX directory_events_expiry ON directory_events(expires);
CREATE TABLE logouts(key TEXT PRIMARY KEY, value TEXT NOT NULL, expires INTEGER NOT NULL) STRICT, WITHOUT ROWID;
CREATE INDEX logouts_expiry ON logouts(expires);
CREATE TABLE sign_ons(key TEXT PRIMARY KEY, expires INTEGER NOT NULL) STRICT, WITHOUT ROWID;
CREATE INDEX sign_ons_expiry ON sign_ons(expires);
CREATE TABLE release_floors(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT, WITHOUT ROWID;
CREATE TABLE recovery_floors(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT, WITHOUT ROWID;
CREATE TABLE recovery_barriers(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT, WITHOUT ROWID;
CREATE TABLE lifecycle_records(
  name TEXT PRIMARY KEY CHECK(name IN ('recoveryRepair','recoveryChallenge','recoveryReceipt','restoreRelease')),
  json TEXT NOT NULL) STRICT, WITHOUT ROWID;
CREATE TRIGGER accounts_no_delete BEFORE DELETE ON accounts BEGIN SELECT RAISE(ABORT, 'native authority rows are never deleted'); END;
CREATE TRIGGER mailboxes_no_delete BEFORE DELETE ON mailboxes BEGIN SELECT RAISE(ABORT, 'native authority rows are never deleted'); END;
CREATE TRIGGER addresses_no_delete BEFORE DELETE ON addresses BEGIN SELECT RAISE(ABORT, 'native authority rows are never deleted'); END;
CREATE TRIGGER address_history_no_delete BEFORE DELETE ON address_history BEGIN SELECT RAISE(ABORT, 'native authority rows are never deleted'); END;
CREATE TRIGGER address_history_no_update BEFORE UPDATE ON address_history BEGIN SELECT RAISE(ABORT, 'address history is append-only'); END;
CREATE TRIGGER address_history_increases BEFORE INSERT ON address_history
  WHEN NEW.generation <= COALESCE((SELECT max(generation) FROM address_history WHERE address = NEW.address), 0)
  BEGIN SELECT RAISE(ABORT, 'address history generations only increase'); END;
CREATE TRIGGER addresses_generation_no_decrease BEFORE UPDATE OF generation ON addresses
  WHEN NEW.generation < OLD.generation
  BEGIN SELECT RAISE(ABORT, 'address generations never decrease'); END;
CREATE TRIGGER addresses_identity BEFORE UPDATE OF address ON addresses
  WHEN NEW.address IS NOT OLD.address
  BEGIN SELECT RAISE(ABORT, 'address identity is immutable'); END;
CREATE TRIGGER mailboxes_identity BEFORE UPDATE OF id, issuer, subject, kind, state_root, source ON mailboxes
  WHEN NEW.id IS NOT OLD.id OR NEW.issuer IS NOT OLD.issuer OR NEW.subject IS NOT OLD.subject OR NEW.kind IS NOT OLD.kind
    OR NEW.state_root IS NOT OLD.state_root OR OLD.source <> '' AND NEW.source IS NOT OLD.source
  BEGIN SELECT RAISE(ABORT, 'mailbox identity is immutable'); END;
CREATE TRIGGER accounts_identity BEFORE UPDATE OF issuer, subject, primary_mailbox ON accounts
  WHEN NEW.issuer IS NOT OLD.issuer OR NEW.subject IS NOT OLD.subject OR NEW.primary_mailbox IS NOT OLD.primary_mailbox
  BEGIN SELECT RAISE(ABORT, 'account identity is immutable'); END;
CREATE TRIGGER accounts_primary_mailbox BEFORE INSERT ON accounts
  WHEN NOT EXISTS(SELECT 1 FROM mailboxes WHERE id = NEW.primary_mailbox AND kind = 'primary' AND issuer = NEW.issuer AND subject = NEW.subject)
  BEGIN SELECT RAISE(ABORT, 'an account names its own primary mailbox'); END;
PRAGMA user_version = 3;
`

// authorityTablesV3 is every table, in an order whose inserts satisfy the
// triggers and immediate foreign keys (mailboxes before accounts and
// addresses, addresses before their history).
var authorityTablesV3 = []string{"meta", "directory", "directory_primary_emails", "mailboxes", "accounts", "addresses", "address_history", "routes_pending", "directory_events", "logouts", "sign_ons", "release_floors", "recovery_floors", "recovery_barriers", "lifecycle_records"}

// authorityRows are v3 rows by table, each row its column values in schema
// order as SQLite returns them: string, int64 or nil.
type authorityRows map[string][][]any

func (r authorityRows) add(table string, values ...any) { r[table] = append(r[table], values) }

func flag(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// foldKey is the alias rule's key: strings.EqualFold(TrimSpace(a), b) holds
// exactly when the keys are equal, because EqualFold compares rune by rune
// within simple-fold orbits and the key takes each orbit's least rune
// (finding H2). Invalid UTF-8 decodes to U+FFFD on both sides.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// primaryEmails is directory_primary_emails derived from the directory.
func primaryEmails(directory map[string]DirectoryState) map[[3]string]bool {
	out := map[[3]string]bool{}
	for key, d := range directory {
		issuer, subject, _ := strings.Cut(key, "\x00")
		if d.Resource == nil {
			continue
		}
		for _, e := range d.Resource.Emails {
			if e.Primary {
				out[[3]string{foldKey(e.Value), issuer, subject}] = true
			}
		}
	}
	return out
}

// authorityV3Rows is the pure v2 → v3 conversion. l is nil when there is no
// ledger file. The ledger must be a validated v2 ledger with authoritative
// generations (callers seed older ones first) behind a set initialization flag.
func authorityV3Rows(life lifecycleFile, l *nativeLedger) (authorityRows, error) {
	if l == nil && life.NativeProvisioningInitialized {
		return nil, ErrNativeProvisioning
	}
	if l != nil {
		if !life.NativeProvisioningInitialized || l.Version != 2 || !l.AddressGenerations {
			return nil, ErrNativeMigration
		}
		if _, _, err := validateNativeLedger(*l); err != nil {
			return nil, err
		}
	}
	var err error
	js := func(v any) string {
		raw, e := json.Marshal(v)
		err = cmp.Or(err, e)
		return string(raw)
	}
	r := authorityRows{}
	r.add("meta", "change", "0")
	r.add("meta", "format", "3")
	r.add("meta", "initialized", strconv.FormatInt(flag(life.NativeProvisioningInitialized), 10))
	for key, d := range life.Directory {
		issuer, subject, ok := strings.Cut(key, "\x00")
		if !ok {
			return nil, ErrNativeProvisioning
		}
		var resource any
		if d.Resource != nil {
			resource = js(d.Resource)
		}
		r.add("directory", issuer, subject, d.Revision, d.Digest, flag(d.Active), d.EventID, d.RevokedBefore, resource)
	}
	for k := range primaryEmails(life.Directory) {
		r.add("directory_primary_emails", k[0], k[1], k[2])
	}
	if l != nil {
		for id, m := range l.Mailboxes {
			r.add("mailboxes", id, m.Owner.Issuer, m.Owner.Subject, m.Kind, m.State, m.StateRoot, m.Limits.MessageBytes, m.Limits.PayloadBytes, int64(m.Limits.Records), m.Source)
		}
		for key, a := range l.Accounts {
			issuer, subject, _ := strings.Cut(key, "\x00") // validated: key is the owner's
			r.add("accounts", issuer, subject, a.Revision, a.Digest, flag(a.DesiredActive), a.Status, a.Failure, a.PrimaryMailbox, flag(a.LegacyMixedUse))
		}
		for address, x := range l.Addresses {
			r.add("addresses", address, x.Mailbox, x.Kind, x.State, x.Generation)
			for _, h := range x.History {
				r.add("address_history", address, h.Generation, h.Mailbox)
			}
		}
	}
	for k, e := range life.Events {
		r.add("directory_events", k, js(e), e.ExpiresAt)
	}
	for k, e := range life.Logouts {
		r.add("logouts", k, js(e), e.RetainUntil)
	}
	for k, exp := range life.SignOns {
		r.add("sign_ons", k, exp)
	}
	for k, v := range life.ReleaseFloors {
		r.add("release_floors", k, js(v))
	}
	for k, v := range life.RecoveryFloors {
		r.add("recovery_floors", k, js(v))
	}
	for k, v := range life.RecoveryRepairBarriers {
		r.add("recovery_barriers", k, js(v))
	}
	for name, v := range map[string]any{"recoveryRepair": life.RecoveryRepair, "recoveryChallenge": life.RecoveryChallenge, "recoveryReceipt": life.RecoveryReceipt, "restoreRelease": life.RestoreRelease} {
		if !reflect.ValueOf(v).IsNil() {
			r.add("lifecycle_records", name, js(v))
		}
	}
	return r, err
}

// rowDecoder reads typed values from rows; the first mismatch sticks.
type rowDecoder struct{ err error }

func (d *rowDecoder) str(v any) string {
	s, ok := v.(string)
	if !ok {
		d.err = cmp.Or(d.err, ErrNativeProvisioning)
	}
	return s
}

func (d *rowDecoder) int(v any) int64 {
	i, ok := v.(int64)
	if !ok {
		d.err = cmp.Or(d.err, ErrNativeProvisioning)
	}
	return i
}

func (d *rowDecoder) bool(v any) bool {
	i := d.int(v)
	if i != 0 && i != 1 {
		d.err = cmp.Or(d.err, ErrNativeProvisioning)
	}
	return i == 1
}

func (d *rowDecoder) json(v any, dst any) {
	if err := json.Unmarshal([]byte(d.str(v)), dst); err != nil {
		d.err = cmp.Or(d.err, ErrNativeProvisioning)
	}
}

func decodeMap[V any](d *rowDecoder, rows [][]any) map[string]V {
	if len(rows) == 0 {
		return nil
	}
	m := map[string]V{}
	for _, row := range rows {
		var v V
		d.json(row[1], &v)
		m[d.str(row[0])] = v
	}
	return m
}

// decodeLifecycle (rows) is the inverse of authorityV3Rows: the lifecycle
// record and, when initialized, the v2-shaped ledger.
func (r authorityRows) decode() (lifecycleFile, *nativeLedger, error) {
	d := &rowDecoder{}
	life := lifecycleFile{}
	meta := map[string]string{}
	for _, row := range r["meta"] {
		meta[d.str(row[0])] = d.str(row[1])
	}
	change, changeErr := strconv.ParseInt(meta["change"], 10, 64)
	if len(meta) != 3 || meta["format"] != "3" || meta["initialized"] != "0" && meta["initialized"] != "1" || changeErr != nil || change < 0 {
		return life, nil, ErrNativeMigration
	}
	life.NativeProvisioningInitialized = meta["initialized"] == "1"
	life.ensureMaps()
	for _, row := range r["directory"] {
		s := DirectoryState{Revision: d.int(row[2]), Digest: d.str(row[3]), Active: d.bool(row[4]), EventID: d.str(row[5]), RevokedBefore: d.int(row[6])}
		if row[7] != nil {
			s.Resource = &DirectoryUser{}
			d.json(row[7], s.Resource)
		}
		life.Directory[directoryKey(d.str(row[0]), d.str(row[1]))] = s
	}
	stored := map[[3]string]bool{}
	for _, row := range r["directory_primary_emails"] {
		stored[[3]string{d.str(row[0]), d.str(row[1]), d.str(row[2])}] = true
	}
	if !reflect.DeepEqual(stored, primaryEmails(life.Directory)) {
		return life, nil, ErrNativeProvisioning // the alias rule's index must match the directory
	}
	for _, row := range r["directory_events"] {
		var e directoryEvent
		d.json(row[1], &e)
		if e.ExpiresAt != d.int(row[2]) {
			return life, nil, ErrNativeProvisioning
		}
		life.Events[d.str(row[0])] = e
	}
	for _, row := range r["logouts"] {
		var e LogoutEvent
		d.json(row[1], &e)
		if e.RetainUntil != d.int(row[2]) {
			return life, nil, ErrNativeProvisioning
		}
		life.Logouts[d.str(row[0])] = e
	}
	for _, row := range r["sign_ons"] {
		life.SignOns[d.str(row[0])] = d.int(row[1])
	}
	life.ReleaseFloors = decodeMap[NativeReleaseFloor](d, r["release_floors"])
	life.RecoveryFloors = decodeMap[int64](d, r["recovery_floors"])
	life.RecoveryRepairBarriers = decodeMap[nativeRecoveryRepairBarrier](d, r["recovery_barriers"])
	for _, row := range r["lifecycle_records"] {
		dst := map[string]any{"recoveryRepair": &life.RecoveryRepair, "recoveryChallenge": &life.RecoveryChallenge, "recoveryReceipt": &life.RecoveryReceipt, "restoreRelease": &life.RestoreRelease}[d.str(row[0])]
		if dst == nil {
			return life, nil, ErrNativeProvisioning
		}
		d.json(row[1], dst)
	}
	if len(r["mailboxes"])+len(r["accounts"])+len(r["addresses"])+len(r["address_history"])+len(r["routes_pending"]) == 0 && !life.NativeProvisioningInitialized {
		return life, nil, d.err
	}
	if !life.NativeProvisioningInitialized {
		return life, nil, ErrNativeProvisioning // ledger rows behind an unset flag
	}
	l := &nativeLedger{Version: 2, AddressGenerations: true, Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}
	for _, row := range r["mailboxes"] {
		m := nativeLedgerMailbox{Kind: d.str(row[3]), State: d.str(row[4]), StateRoot: d.str(row[5]), Source: d.str(row[9])}
		m.Owner.Issuer, m.Owner.Subject = d.str(row[1]), d.str(row[2])
		m.Limits.MessageBytes, m.Limits.PayloadBytes, m.Limits.Records = d.int(row[6]), d.int(row[7]), int(d.int(row[8]))
		l.Mailboxes[d.str(row[0])] = m
	}
	for _, row := range r["accounts"] {
		l.Accounts[directoryKey(d.str(row[0]), d.str(row[1]))] = nativeLedgerAccount{Revision: d.int(row[2]), Digest: d.str(row[3]), DesiredActive: d.bool(row[4]), Status: d.str(row[5]), Failure: d.str(row[6]), PrimaryMailbox: d.str(row[7]), LegacyMixedUse: d.bool(row[8])}
	}
	for _, row := range r["addresses"] {
		l.Addresses[d.str(row[0])] = nativeLedgerAddress{Mailbox: d.str(row[1]), Kind: d.str(row[2]), State: d.str(row[3]), Generation: d.int(row[4])}
	}
	for _, row := range r["address_history"] {
		address := d.str(row[0])
		x, ok := l.Addresses[address]
		if !ok {
			return life, nil, ErrNativeProvisioning
		}
		x.History = append(x.History, nativeAddressHistory{Mailbox: d.str(row[2]), Generation: d.int(row[1])})
		l.Addresses[address] = x
	}
	for address, x := range l.Addresses {
		slices.SortFunc(x.History, func(a, b nativeAddressHistory) int { return cmp.Compare(a.Generation, b.Generation) })
		l.Addresses[address] = x
	}
	return life, l, d.err
}

// normalizedSchema lists sqlite_schema with whitespace collapsed, rootpage aside.
func normalizedSchema(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, table, text string
		if err := rows.Scan(&typ, &name, &table, &text); err != nil {
			return nil, err
		}
		out = append(out, strings.Join([]string{typ, name, table, strings.Join(strings.Fields(text), " ")}, "\x00"))
	}
	return out, rows.Err()
}

// referenceSchemaV3 is sqlite_schema of a fresh database built from authoritySchemaV3.
func referenceSchemaV3(ctx context.Context) ([]string, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, authoritySchemaV3); err != nil {
		return nil, err
	}
	return normalizedSchema(ctx, db)
}

// readAuthorityV3Historical opens a staged v3 database read-only and immutable
// and returns its rows. It refuses (ErrNativeMigration) unless the file is a
// private regular file, the handle runs with trusted_schema=OFF,
// recursive_triggers=ON, query_only and defensive mode, user_version is 3,
// sqlite_schema matches authoritySchemaV3 exactly, and integrity_check and
// foreign_key_check pass,
// all before any row is read (findings S2, S3).
func readAuthorityV3Historical(ctx context.Context, path string) (authorityRows, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrNativeMigration
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=ro&immutable=1&_defensive=1&_pragma=trusted_schema(0)&_pragma=recursive_triggers(1)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var trusted, recursive, queryOnly, defensive, version int
	var integrity string
	if err := db.QueryRowContext(ctx, "SELECT (SELECT trusted_schema FROM pragma_trusted_schema), (SELECT recursive_triggers FROM pragma_recursive_triggers), (SELECT query_only FROM pragma_query_only), (SELECT user_version FROM pragma_user_version)").Scan(&trusted, &recursive, &queryOnly, &version); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNativeMigration, err)
	}
	// Defensive mode leaves writable_schema off whatever is asked.
	if _, err := db.ExecContext(ctx, "PRAGMA writable_schema=ON"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNativeMigration, err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA writable_schema").Scan(&defensive); err != nil || trusted != 0 || recursive != 1 || queryOnly != 1 || defensive != 0 || version != 3 {
		return nil, ErrNativeMigration
	}
	want, err := referenceSchemaV3(ctx)
	if err != nil {
		return nil, err
	}
	got, err := normalizedSchema(ctx, db)
	if err != nil || !slices.Equal(got, want) {
		return nil, ErrNativeMigration
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, ErrNativeMigration
	}
	// integrity_check covers storage, not foreign keys, which a writer with
	// foreign_keys off could have broken.
	var violation any
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_key_check").Scan(&violation); !errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNativeMigration
	}
	r := authorityRows{}
	for _, table := range authorityTablesV3 {
		if err := readTable(ctx, db, table, r); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNativeMigration, err)
		}
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) {
		return nil, ErrNativeMigration
	}
	return r, nil
}

func readTable(ctx context.Context, db *sql.DB, table string, r authorityRows) error {
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+table) // fixed table names only
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		values := make([]any, len(columns))
		dst := make([]any, len(columns))
		for i := range values {
			dst[i] = &values[i]
		}
		if err := rows.Scan(dst...); err != nil {
			return err
		}
		r.add(table, values...)
	}
	return rows.Err()
}

// jsonStub reports whether the file at path is exactly the stub want.
func jsonStub(path string, want map[string]any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var got map[string]any
	return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, want), nil
}

// historicalAuthority reads a snapshot's lifecycle and ledger in either
// storage format. Without native-authority.db it is the v1/v2 file reader
// (which refuses the stubs). With it, both JSON files must be the exact v3
// stubs; any other mix is refused. The version is the ledger's (0 when
// absent; 3 for an initialized v3 database).
func (s *LifecycleStore) historicalAuthority() (lifecycleFile, nativeAssignments, int, error) {
	dir := filepath.Dir(s.path)
	if _, err := os.Lstat(filepath.Join(dir, NativeAuthorityFile)); errors.Is(err, os.ErrNotExist) {
		lifecycle, err := s.load()
		if err != nil {
			return lifecycle, nativeAssignments{}, 0, err
		}
		f, version, err := s.readNativeLedger(lifecycle, true)
		return lifecycle, f, version, err
	}
	for path, stub := range map[string]map[string]any{s.path: lifecycleStubV3, s.nativePath(): ledgerStubV3} {
		if ok, err := jsonStub(path, stub); err != nil || !ok {
			return lifecycleFile{}, nativeAssignments{}, 0, ErrNativeMigration
		}
	}
	rows, err := readAuthorityV3Historical(context.Background(), filepath.Join(dir, NativeAuthorityFile))
	if err != nil {
		return lifecycleFile{}, nativeAssignments{}, 0, err
	}
	life, l, err := rows.decode()
	if err != nil || l == nil {
		return life, nativeAssignments{Accounts: map[string]NativeAssignment{}}, 0, err
	}
	f, _, err := validateNativeLedger(*l)
	return life, f, 3, err
}

// writeAuthorityV3 creates path (which must not exist) with schema and rows,
// in one transaction. Only tests call it until the SQLite backend lands.
func writeAuthorityV3(ctx context.Context, path, schema string, r authorityRows) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=rw&_txlock=immediate&_defensive=1&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	for _, table := range authorityTablesV3 {
		for _, row := range r[table] {
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")+")", row...); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
		}
	}
	return tx.Commit()
}

// WriteNativeAuthorityV3ForTest converts configDir's v2 lifecycle and ledger
// into native-authority.db plus both stubs, as migration will. No binary
// writes v3 yet, so this is a test fixture only.
func WriteNativeAuthorityV3ForTest(configDir string) error {
	if !testing.Testing() {
		panic("v3 native authority fixture outside tests")
	}
	s := NewLifecycleStore(configDir)
	life, err := s.load()
	if err != nil {
		return err
	}
	var l *nativeLedger
	if raw, err := os.ReadFile(s.nativePath()); err == nil {
		l = &nativeLedger{}
		if err := json.Unmarshal(raw, l); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rows, err := authorityV3Rows(life, l)
	if err != nil {
		return err
	}
	if err := writeAuthorityV3(context.Background(), filepath.Join(configDir, NativeAuthorityFile), authoritySchemaV3, rows); err != nil {
		return err
	}
	// The ledger stub last, as migration writes it.
	for _, stub := range []struct {
		path string
		doc  map[string]any
	}{{s.path, lifecycleStubV3}, {s.nativePath(), ledgerStubV3}} {
		raw, err := json.Marshal(stub.doc)
		if err == nil {
			err = os.WriteFile(stub.path, raw, 0600)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
