// Package ingress holds accepted incoming mail until an authenticated local
// importer has committed all intended mailbox deliveries. It exposes no network
// authentication or production receiver configuration.
package ingress

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict     = errors.New("ingress identity conflict; preserve the receipt and investigate")
	ErrCapacity     = errors.New("ingress capacity reached; let waiting mail import or reconcile receiving storage before accepting more mail")
	ErrRoute        = errors.New("recipient is unknown, disabled or routing is stale; reconcile the directory")
	ErrRoutingStale = errors.New("routing snapshot expired; refresh it before accepting mail")
	ErrLease        = errors.New("ingress claim expired or changed; reacquire before acknowledging")
	// ErrNotQuarantined refuses release/discard of anything else, or while a
	// release holds the delivery.
	ErrNotQuarantined = errors.New("delivery is not quarantined, or a release is in progress; list quarantine again")
)

// ReceivingLimits are the durable holding-store limits every opener of the
// production spool must pass: a 25 MiB message (both receiving profiles), a
// 512 MiB burst of held mail and 10,000 held deliveries. An opener raises
// lower durable limits to these; it never lowers them.
var ReceivingLimits = Limits{MessageBytes: 25 << 20, PayloadBytes: 512 << 20, Records: 10000}

type Limits struct {
	MessageBytes int64
	PayloadBytes int64
	Records      int
}

type Route struct {
	Address    string
	Issuer     string
	Subject    string
	Mailbox    string
	Generation int64
	Active     bool
	ValidUntil time.Time
}

type Binding struct {
	Address    string
	Issuer     string
	Subject    string
	Mailbox    string
	Generation int64
}

type Delivery struct {
	Gateway  string
	ID       string
	Sender   string
	Digest   string
	Raw      []byte
	State    string
	Bindings []Binding
	Lease    string
	// Disposition of an archived delivery: imported, released or discarded.
	Disposition string
}

// Quarantined is a quarantined delivery's envelope, never its bytes.
type Quarantined struct {
	Sequence int64
	Gateway  string
	ID       string
	Sender   string
	Received time.Time
	Size     int64
	Bindings []Binding
}

type Summary struct {
	Sequence int64
	ID       string
	State    string
}

type Store struct {
	db            *sql.DB
	limits        Limits
	path          string
	physicalLimit int64
}

const schema = `
CREATE TABLE IF NOT EXISTS limits (id INTEGER PRIMARY KEY CHECK(id=1), message_bytes INTEGER, payload_bytes INTEGER, records INTEGER);
CREATE TABLE IF NOT EXISTS routes (address TEXT PRIMARY KEY, issuer TEXT NOT NULL, subject TEXT NOT NULL, mailbox TEXT NOT NULL, generation INTEGER NOT NULL, active INTEGER NOT NULL, valid_until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS deliveries (gateway TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, created INTEGER NOT NULL, digest TEXT NOT NULL DEFAULT '', raw BLOB, state TEXT NOT NULL DEFAULT 'staged', lease TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(gateway,id));
CREATE TABLE IF NOT EXISTS bindings (gateway TEXT NOT NULL, id TEXT NOT NULL, address TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL, mailbox TEXT NOT NULL, generation INTEGER NOT NULL, PRIMARY KEY(gateway,id,address), FOREIGN KEY(gateway,id) REFERENCES deliveries(gateway,id) ON DELETE CASCADE);
`

// Acknowledged, released and discarded deliveries become tombstones outside
// the record limit: enough to recognise an exact replay, never enough to
// deliver again. archived_at (UTC unix seconds) lets a future age pruner be one
// DELETE; disposition tells audit how the delivery ended. The partial index
// keeps the per-open legacy check off the payload pages.
const archivedSchema = `CREATE TABLE IF NOT EXISTS archived (gateway TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, digest TEXT NOT NULL, recipients TEXT NOT NULL, archived_at INTEGER NOT NULL, disposition TEXT NOT NULL CHECK(disposition IN ('imported','released','discarded','partially_released')), PRIMARY KEY(gateway,id)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS imported ON deliveries(gateway,id) WHERE state='imported';`

// ArchivedColumns is the tombstone table shape restore validation expects.
const ArchivedColumns = "gateway TEXT,id TEXT,sender TEXT,digest TEXT,recipients TEXT,archived_at INTEGER,disposition TEXT"

// archive moves deliveries matching where into tombstones inside the caller's
// writer transaction; bindings cascade with the delivery row. insert is
// "INSERT" (a duplicate is a bug) or "INSERT OR IGNORE" (legacy).
func archive(ctx context.Context, tx *sql.Tx, insert, disposition, where string, args ...any) error {
	_, err := tx.ExecContext(ctx, insert+` INTO archived(gateway,id,sender,digest,recipients,archived_at,disposition) SELECT gateway,id,sender,digest,(SELECT group_concat(b.address,char(10)) FROM bindings b WHERE b.gateway=d.gateway AND b.id=d.id),unixepoch(),? FROM deliveries d WHERE `+where, append([]any{disposition}, args...)...)
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM deliveries WHERE "+where, args...)
	}
	return err
}

// migrateArchive creates the tombstone table and archives 'imported' rows left
// by an earlier (or downgraded) binary. A plain read skips the writer lock when
// there is nothing to do; the write is one idempotent transaction.
func migrateArchive(ctx context.Context, db *sql.DB) error {
	var ready, legacy, disposition int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name IN ('archived','imported')").Scan(&ready); err != nil {
		return err
	}
	if ready == 2 {
		if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM deliveries INDEXED BY imported WHERE state='imported'),(SELECT COUNT(*) FROM pragma_table_info('archived') WHERE name='disposition')").Scan(&legacy, &disposition); err != nil || legacy == 0 && disposition == 1 {
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, archivedSchema); err != nil {
		return err
	}
	// Tombstones written before dispositions existed were all imports.
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('archived') WHERE name='disposition'").Scan(&disposition); err != nil {
		return err
	}
	if disposition == 0 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE archived ADD COLUMN disposition TEXT NOT NULL DEFAULT 'imported' CHECK(disposition IN ('imported','released','discarded','partially_released'))"); err != nil {
			return err
		}
	}
	if err := archive(ctx, tx, "INSERT OR IGNORE", "imported", "state='imported'"); err != nil {
		return err
	}
	return tx.Commit()
}

type tombstone struct {
	sender, digest, disposition string
	recipients                  []string
}

func archivedDelivery(ctx context.Context, tx *sql.Tx, gateway, id string) (tombstone, error) {
	var t tombstone
	var recipients string
	err := tx.QueryRowContext(ctx, "SELECT sender,digest,disposition,recipients FROM archived WHERE gateway=? AND id=?", gateway, id).Scan(&t.sender, &t.digest, &t.disposition, &recipients)
	t.recipients = strings.Split(recipients, "\n")
	return t, err
}

func Open(dir string, limits Limits) (*Store, error) {
	return open(dir, limits, false)
}

// OpenExisting is the runtime opener. Only explicit initialization may create
// a holding database; missing accepted-mail storage must never look empty.
func OpenExisting(dir string, limits Limits) (*Store, error) {
	return open(dir, limits, true)
}

func open(dir string, limits Limits, existing bool) (*Store, error) {
	if limits.MessageBytes <= 0 || limits.MessageBytes > 64<<20 || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 || limits.PayloadBytes > math.MaxInt64/4 || int64(limits.Records) > (math.MaxInt64-4*limits.PayloadBytes)/(32<<10) {
		return nil, errors.New("invalid ingress limits")
	}
	if !existing {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("ingress directory must be owner-only")
	}
	path, err := filepath.Abs(filepath.Join(dir, "ingress.db"))
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("_txlock", "immediate")
	query["_pragma"] = []string{"synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)"}
	if existing {
		query.Set("mode", "rw")
	} else {
		query["_pragma"] = append(query["_pragma"], "journal_mode(WAL)")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, limits: limits, path: path, physicalLimit: max(32<<20, 4*limits.PayloadBytes+int64(limits.Records)*(32<<10))}
	if existing {
		var persisted Limits
		err := db.QueryRow("SELECT message_bytes,payload_bytes,records FROM limits WHERE id=1").Scan(&persisted.MessageBytes, &persisted.PayloadBytes, &persisted.Records)
		if err == nil && !persisted.raisableTo(limits) {
			err = errLimits
		}
		if err == nil {
			var tables int
			err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('routes','deliveries','bindings')").Scan(&tables)
			if err == nil && tables != 3 {
				err = errors.New("receiving storage is incomplete; preserve it and reconcile")
			}
		}
		if err == nil {
			_, err = db.Exec("PRAGMA journal_mode=WAL")
		}
		if err == nil && persisted != limits {
			err = raiseLimits(db, limits)
		}
		if err == nil {
			err = migrateArchive(context.Background(), db)
		}
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	}
	tx, err := db.Begin()
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(schema)
		if err == nil {
			_, err = tx.Exec("INSERT OR IGNORE INTO limits VALUES(1,?,?,?)", limits.MessageBytes, limits.PayloadBytes, limits.Records)
		}
		if err == nil {
			err = adoptLimits(tx, limits)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err == nil {
		err = migrateArchive(context.Background(), db)
	}
	if err == nil {
		err = fsutil.SyncDir(dir)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// HeldBytes is the accepted mail still waiting to import into mailbox: what
// it will store once imported, so admission counts it against the quota.
// ponytail: no index on bindings(mailbox); the scan is bounded by the record
// limit (10,000 held deliveries). Index it if that limit grows.
func (s *Store) HeldBytes(ctx context.Context, mailbox string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(sum(length(d.raw)),0) FROM deliveries d JOIN bindings b ON b.gateway=d.gateway AND b.id=d.id WHERE b.mailbox=? AND d.state='pending'", mailbox).Scan(&n)
	return n, err
}

var errLimits = errors.New("ingress limits differ from durable configuration and would lower them; run the newer release")

// raisableTo reports whether every durable limit is at most the configured one.
func (l Limits) raisableTo(c Limits) bool {
	return l.MessageBytes <= c.MessageBytes && l.PayloadBytes <= c.PayloadBytes && l.Records <= c.Records
}

func raiseLimits(db *sql.DB, limits Limits) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = adoptLimits(tx, limits); err != nil {
		return err
	}
	return tx.Commit()
}

// adoptLimits raises the durable limits to limits inside the writer
// transaction, one atomic row update, so a crash leaves old or new limits and
// the next open completes it. Lowering is refused: held mail may exceed it.
func adoptLimits(tx *sql.Tx, limits Limits) error {
	var persisted Limits
	if err := tx.QueryRow("SELECT message_bytes,payload_bytes,records FROM limits WHERE id=1").Scan(&persisted.MessageBytes, &persisted.PayloadBytes, &persisted.Records); err != nil {
		return err
	}
	if persisted == limits {
		return nil
	}
	if !persisted.raisableTo(limits) {
		return errLimits
	}
	_, err := tx.Exec("UPDATE limits SET message_bytes=?,payload_bytes=?,records=? WHERE id=1", limits.MessageBytes, limits.PayloadBytes, limits.Records)
	return err
}

func address(v string, allowEmpty bool) bool {
	if allowEmpty && v == "" {
		return true
	}
	if len(v) > 320 || strings.ContainsAny(v, "\r\n\x00") {
		return false
	}
	a, err := mail.ParseAddress(v)
	return err == nil && a.Name == "" && a.Address == v
}

// ValidIdentifier reports whether v can be a gateway or delivery ID.
func ValidIdentifier(v string) bool { return identifier(v) }

func identifier(v string) bool {
	return v != "" && len(v) <= 256 && !strings.ContainsAny(v, "\x00\r\n")
}

// SetRoute accepts only an already verified directory decision from a trusted
// writer. It does not verify a JWT or establish domain ownership.
func (s *Store) SetRoute(ctx context.Context, r Route) error {
	return s.setRoute(ctx, r, false)
}

// RefreshRoute renews an existing unchanged route for accepted-mail recovery.
// It cannot create a route, change its generation/owner, or reactivate it.
func (s *Store) RefreshRoute(ctx context.Context, r Route) error {
	return s.setRoute(ctx, r, true)
}

func (s *Store) setRoute(ctx context.Context, r Route, recovery bool) error {
	if !address(r.Address, false) || !identifier(r.Issuer) || !identifier(r.Subject) || !identifier(r.Mailbox) || r.Generation <= 0 || r.ValidUntil.IsZero() {
		return ErrRoute
	}
	var tx *sql.Tx
	var err error
	if recovery {
		tx, err = s.db.BeginTx(ctx, nil)
	} else {
		tx, err = s.admissionTx(ctx)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var old Route
	err = tx.QueryRowContext(ctx, "SELECT issuer,subject,mailbox,generation,active,valid_until FROM routes WHERE address=?", r.Address).Scan(&old.Issuer, &old.Subject, &old.Mailbox, &old.Generation, &old.Active, new(int64))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (r.Generation < old.Generation || (r.Generation == old.Generation && (r.Issuer != old.Issuer || r.Subject != old.Subject || r.Mailbox != old.Mailbox || r.Active != old.Active))) {
		return ErrConflict
	}
	if recovery && (err != nil || r.Issuer != old.Issuer || r.Subject != old.Subject || r.Mailbox != old.Mailbox || r.Generation != old.Generation || r.Active != old.Active) {
		return ErrConflict
	}
	if !recovery {
		if err := s.checkAdmission(ctx, tx, 0); err != nil {
			return err
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM routes").Scan(&count); err != nil {
			return err
		}
		if count >= s.limits.Records {
			return ErrCapacity
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO routes VALUES(?,?,?,?,?,?,?) ON CONFLICT(address) DO UPDATE SET issuer=excluded.issuer,subject=excluded.subject,mailbox=excluded.mailbox,generation=excluded.generation,active=excluded.active,valid_until=excluded.valid_until", r.Address, r.Issuer, r.Subject, r.Mailbox, r.Generation, r.Active, r.ValidUntil.Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeactivateRoutes writes existing routes inactive at the given generations,
// for a trusted ledger writer holding the directory fence (release, disable,
// reassign). A generation below the stored one is a conflict. A missing store
// or route is a no-op: every bind writes the current route first.
func DeactivateRoutes(ctx context.Context, dir string, generations map[string]int64) error {
	if len(generations) == 0 {
		return nil
	}
	path, err := filepath.Abs(filepath.Join(dir, "ingress.db"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("_txlock", "immediate")
	query.Set("mode", "rw")
	query["_pragma"] = []string{"synchronous(FULL)", "busy_timeout(5000)"}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for address, generation := range generations {
		var old int64
		var active bool
		err := tx.QueryRowContext(ctx, "SELECT generation,active FROM routes WHERE address=?", address).Scan(&old, &active)
		if errors.Is(err, sql.ErrNoRows) || err == nil && old == generation && !active {
			continue
		}
		if err != nil {
			return err
		}
		if old > generation {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE routes SET generation=?,active=0 WHERE address=?", generation, address); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Bind freezes a successful RCPT decision before returning to the receiver.
// deliveryID must be generated by the trusted receiver, never Message-ID.
func (s *Store) Bind(ctx context.Context, gateway, deliveryID, sender, recipient string) error {
	if !identifier(gateway) || !identifier(deliveryID) || !address(sender, true) || !address(recipient, false) {
		return ErrConflict
	}
	tx, err := s.admissionTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// ponytail: no age-based deletion, including staged transactions. SMTP can
	// still be active; erasing earlier RCPT bindings would acknowledge partial
	// delivery. Finite records refuse admission until explicit fenced cleanup.
	var oldSender, state string
	err = tx.QueryRowContext(ctx, "SELECT sender,state FROM deliveries WHERE gateway=? AND id=?", gateway, deliveryID).Scan(&oldSender, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil {
		// An archived delivery is complete: replay its exact RCPT, never reopen it.
		t, errArchived := archivedDelivery(ctx, tx, gateway, deliveryID)
		if errArchived == nil {
			if t.sender != sender || !slices.Contains(t.recipients, recipient) {
				return ErrConflict
			}
			return tx.Commit()
		}
		if !errors.Is(errArchived, sql.ErrNoRows) {
			return errArchived
		}
	}
	if err == nil {
		if oldSender != sender {
			return ErrConflict
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM bindings WHERE gateway=? AND id=? AND address=?", gateway, deliveryID, recipient).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return tx.Commit()
		}
		if state != "staged" {
			return ErrConflict
		}
	}
	if err := s.checkAdmission(ctx, tx, 0); err != nil {
		return err
	}
	var route Route
	var validUntil int64
	errRoute := tx.QueryRowContext(ctx, "SELECT issuer,subject,mailbox,generation,active,valid_until FROM routes WHERE address=?", recipient).Scan(&route.Issuer, &route.Subject, &route.Mailbox, &route.Generation, &route.Active, &validUntil)
	if errors.Is(errRoute, sql.ErrNoRows) {
		return ErrRoute
	}
	if errRoute != nil {
		return errRoute
	}
	if !route.Active {
		return ErrRoute
	}
	if validUntil <= time.Now().Unix() {
		return ErrRoutingStale
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&count); err != nil {
			return err
		}
		if count >= s.limits.Records {
			return ErrCapacity
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO deliveries(gateway,id,sender,created) VALUES(?,?,?,?)", gateway, deliveryID, sender, time.Now().Unix()); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM bindings WHERE gateway=? AND id=?", gateway, deliveryID).Scan(&count); err != nil {
		return err
	}
	if count >= 100 {
		return ErrCapacity
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO bindings VALUES(?,?,?,?,?,?,?)", gateway, deliveryID, recipient, route.Issuer, route.Subject, route.Mailbox, route.Generation)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Accept commits raw MIME before an upstream acknowledgment. A repeated local
// submission with the same receiver identity is idempotent only for exact bytes.
func (s *Store) Accept(ctx context.Context, gateway, id, sender string, raw io.Reader) error {
	if !identifier(gateway) || !identifier(id) || !address(sender, true) {
		return ErrConflict
	}
	data, err := io.ReadAll(io.LimitReader(raw, s.limits.MessageBytes+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > s.limits.MessageBytes {
		return ErrCapacity
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	tx, err := s.admissionTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldSender, oldDigest, state string
	err = tx.QueryRowContext(ctx, "SELECT sender,digest,state FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&oldSender, &oldDigest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		t, err := archivedDelivery(ctx, tx, gateway, id)
		if err != nil || t.sender != sender || t.digest != digest {
			return ErrConflict
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if oldSender != sender {
		return ErrConflict
	}
	if state != "staged" {
		if oldDigest != digest {
			return ErrConflict
		}
		return tx.Commit()
	}
	if err := s.admitPayload(ctx, tx, int64(len(data))); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET raw=?,digest=?,state='pending' WHERE gateway=? AND id=?", data, digest, gateway, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) admitPayload(ctx context.Context, tx *sql.Tx, size int64) error {
	if err := s.checkAdmission(ctx, tx, size); err != nil {
		return err
	}
	var used int64
	// ponytail: scan at most limits.Records rows; add transactional byte counters
	// only if measured spool throughput makes this bounded scan a bottleneck.
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(raw)),0) FROM deliveries").Scan(&used); err != nil {
		return err
	}
	if size > s.limits.PayloadBytes-used {
		return ErrCapacity
	}
	return nil
}

// Quarantine records a hosted gateway's frozen binding, with its bytes, when
// current authority no longer matches it or it cannot be resolved: never
// imported, kept for release or discard. An all-empty owner means the
// binding could not be resolved (an unknown table revision); release refuses
// it and discard remains. A staged delivery with the same sender and binding
// gains the bytes. An exact replay succeeds.
func (s *Store) Quarantine(ctx context.Context, gateway, id, sender string, b Binding, raw []byte) error {
	if !identifier(gateway) || !identifier(id) || !address(sender, true) || !address(b.Address, false) || b.Generation <= 0 ||
		!b.Unresolved() && (!identifier(b.Issuer) || !identifier(b.Subject) || !identifier(b.Mailbox)) {
		return ErrConflict
	}
	if len(raw) == 0 || int64(len(raw)) > s.limits.MessageBytes {
		return ErrCapacity
	}
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	tx, err := s.admissionTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldSender, oldDigest, state string
	err = tx.QueryRowContext(ctx, "SELECT sender,digest,state FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&oldSender, &oldDigest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		t, err := archivedDelivery(ctx, tx, gateway, id)
		if err == nil {
			if t.sender != sender || t.digest != digest {
				return ErrConflict
			}
			return tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&count); err != nil {
			return err
		}
		if count >= s.limits.Records {
			return ErrCapacity
		}
		if err := s.admitPayload(ctx, tx, int64(len(raw))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO deliveries(gateway,id,sender,created,digest,raw,state) VALUES(?,?,?,?,?,?,'quarantined')", gateway, id, sender, time.Now().Unix(), digest, raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO bindings VALUES(?,?,?,?,?,?,?)", gateway, id, b.Address, b.Issuer, b.Subject, b.Mailbox, b.Generation); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if oldSender != sender {
		return ErrConflict
	}
	if state == "quarantined" {
		if oldDigest != digest {
			return ErrConflict
		}
		return tx.Commit()
	}
	bindings, err := readBindings(ctx, tx, gateway, id)
	if err != nil {
		return err
	}
	// A pending delivery already holds bytes: QuarantinePending fences it.
	if state != "staged" || len(bindings) != 1 || bindings[0] != b {
		return ErrConflict
	}
	if err := s.admitPayload(ctx, tx, int64(len(raw))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE deliveries SET raw=?,digest=?,state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=?", raw, digest, gateway, id); err != nil {
		return err
	}
	return tx.Commit()
}

func readDelivery(ctx context.Context, tx *sql.Tx, gateway, id string) (Delivery, error) {
	d := Delivery{Gateway: gateway, ID: id}
	err := tx.QueryRowContext(ctx, "SELECT sender,digest,raw,state,lease FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&d.Sender, &d.Digest, &d.Raw, &d.State, &d.Lease)
	if errors.Is(err, sql.ErrNoRows) {
		t, err := archivedDelivery(ctx, tx, gateway, id)
		d.Sender, d.Digest, d.State, d.Disposition = t.sender, t.digest, "archived", t.disposition
		return d, err
	}
	if err != nil {
		return d, err
	}
	d.Bindings, err = readBindings(ctx, tx, gateway, id)
	return d, err
}

func readBindings(ctx context.Context, tx *sql.Tx, gateway, id string) ([]Binding, error) {
	rows, err := tx.QueryContext(ctx, "SELECT address,issuer,subject,mailbox,generation FROM bindings WHERE gateway=? AND id=? ORDER BY address", gateway, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.Address, &b.Issuer, &b.Subject, &b.Mailbox, &b.Generation); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// Get is for a trusted local operator/importer, never a user-facing mail API.
func (s *Store) Get(ctx context.Context, gateway, id string) (Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = tx.Rollback() }()
	d, err := readDelivery(ctx, tx, gateway, id)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}

// QuarantinePending retains a proven stale obligation without reassigning it.
// The trusted caller holds current directory/users authority. An active claim
// cannot be invalidated by a second importer that has not acquired its lease.
func (s *Store) QuarantinePending(ctx context.Context, gateway, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE deliveries SET state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=? AND state='pending' AND (lease='' OR lease_until<=?)", gateway, id, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLease
	}
	return nil
}

// List pages staged, pending and quarantined receipts without loading MIME;
// archived deliveries are not listed. Sequence is an enumeration position, not
// a change-sync cursor; importers revisit unacknowledged receipts.
func (s *Store) List(ctx context.Context, gateway string, after int64, limit int) ([]Summary, error) {
	if !identifier(gateway) || after < 0 || limit < 1 || limit > 100 {
		return nil, ErrConflict
	}
	rows, err := s.db.QueryContext(ctx, "SELECT rowid,id,state FROM deliveries WHERE gateway=? AND rowid>? ORDER BY rowid LIMIT ?", gateway, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Summary
	for rows.Next() {
		var item Summary
		if err := rows.Scan(&item.Sequence, &item.ID, &item.State); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func currentBindings(ctx context.Context, tx *sql.Tx, gateway, id string) (bool, error) {
	var total, valid int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN r.active=1 AND r.valid_until>? AND r.issuer=b.issuer AND r.subject=b.subject AND r.mailbox=b.mailbox AND r.generation=b.generation THEN 1 ELSE 0 END),0) FROM bindings b LEFT JOIN routes r ON b.address=r.address WHERE b.gateway=? AND b.id=?`, time.Now().Unix(), gateway, id).Scan(&total, &valid)
	return total > 0 && total == valid, err
}

// Claim fences competing importers. A stale owner binding is retained in
// quarantine, never replaced with the address's current owner.
func (s *Store) Claim(ctx context.Context, gateway, id string, duration time.Duration) (Delivery, error) {
	return s.claim(ctx, gateway, id, duration, false)
}

// claim leases a pending delivery, or for release a quarantined one: release
// skips the route check because the caller has re-admitted the frozen owners.
// The delivery stays quarantined while leased, so the importer never sees it.
func (s *Store) claim(ctx context.Context, gateway, id string, duration time.Duration, release bool) (Delivery, error) {
	want := "pending"
	if release {
		want = "quarantined"
	}
	if duration < time.Second || duration > 5*time.Minute {
		return Delivery{}, ErrLease
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = tx.Rollback() }()
	d, err := readDelivery(ctx, tx, gateway, id)
	if err != nil {
		return d, err
	}
	if d.State != want {
		return d, ErrLease
	}
	var until int64
	if err := tx.QueryRowContext(ctx, "SELECT lease_until FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&until); err != nil {
		return d, err
	}
	if until > time.Now().Unix() {
		return d, ErrLease
	}
	current, err := currentBindings(ctx, tx, gateway, id)
	if err != nil {
		return d, err
	}
	if !current && !release {
		_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=?", gateway, id)
		if err != nil {
			return d, err
		}
		if err := tx.Commit(); err != nil {
			return d, err
		}
		d.State = "quarantined"
		return d, ErrRoute
	}
	d.Lease, err = fsutil.NewUUIDv4()
	if err != nil {
		return d, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET lease=?,lease_until=? WHERE gateway=? AND id=?", d.Lease, time.Now().Add(duration).Unix(), gateway, id)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}

// Acknowledge requires the active claim and exact receipt digest. The caller
// MUST have committed every binding's mailbox receipt first. Lost acknowledgments
// are reconciled using the retained digest, never by resending/deleting blindly.
func (s *Store) Acknowledge(ctx context.Context, gateway, id, lease, digest string) error {
	return s.acknowledge(ctx, gateway, id, lease, digest, false)
}

func (s *Store) acknowledge(ctx context.Context, gateway, id, lease, digest string, release bool) error {
	want, disposition := "pending", "imported"
	if release {
		want, disposition = "quarantined", "released"
	}
	if lease == "" || digest == "" {
		return ErrLease
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state, oldLease, oldDigest string
	var until int64
	err = tx.QueryRowContext(ctx, "SELECT state,lease,digest,lease_until FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&state, &oldLease, &oldDigest, &until)
	if errors.Is(err, sql.ErrNoRows) {
		// Lost local ACK reply: the archived digest is all that remains.
		t, err := archivedDelivery(ctx, tx, gateway, id)
		if err != nil || t.digest != digest || t.disposition != disposition {
			return ErrLease
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if oldDigest != digest || oldLease != lease {
		return ErrLease
	}
	if state != want || until <= time.Now().Unix() {
		return ErrLease
	}
	current, err := currentBindings(ctx, tx, gateway, id)
	if err != nil {
		return err
	}
	if !current && !release {
		_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=?", gateway, id)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return ErrRoute
	}
	// ponytail: tombstones are never pruned. Typically ~0.2 KiB (about 2 million
	// in the default budget; ~170K attacker-shaped), and ingress.db hits the
	// 64 MiB backup file cap near 360K. Pruning by archived_at, or after a
	// gateway proves its source copy gone, is a public-MX gate.
	if err := archive(ctx, tx, "INSERT", disposition, "gateway=? AND id=? AND state=?", gateway, id, want); err != nil {
		return err
	}
	return tx.Commit()
}

// Discard drops a quarantined delivery's bytes and bindings, leaving a
// tombstone so an exact replay or re-pickup cannot resurrect it, and returns
// its disposition. A live release lease refuses; after an interrupted release
// some frozen mailboxes may already hold the mail, so the tombstone says
// partially_released. Repeating a discard succeeds.
func (s *Store) Discard(ctx context.Context, gateway, id string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var state, lease string
	var until int64
	err = tx.QueryRowContext(ctx, "SELECT state,lease,lease_until FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&state, &lease, &until)
	if errors.Is(err, sql.ErrNoRows) {
		t, err := archivedDelivery(ctx, tx, gateway, id)
		if err != nil {
			return "", err
		}
		if t.disposition != "discarded" && t.disposition != "partially_released" {
			return "", ErrNotQuarantined
		}
		return t.disposition, tx.Commit()
	}
	if err != nil {
		return "", err
	}
	if state != "quarantined" || until > time.Now().Unix() {
		return "", ErrNotQuarantined
	}
	// Every path into quarantine clears the lease; only Release sets one.
	disposition := "discarded"
	if lease != "" {
		disposition = "partially_released"
	}
	if err := archive(ctx, tx, "INSERT", disposition, "gateway=? AND id=? AND state='quarantined'", gateway, id); err != nil {
		return "", err
	}
	return disposition, tx.Commit()
}

// Unresolved reports a binding Quarantine recorded without an owner.
func (b Binding) Unresolved() bool { return b.Issuer == "" && b.Subject == "" && b.Mailbox == "" }

// ResolveQuarantined binds an unresolved quarantined delivery's recipient to
// owner b, chosen by an administrator. It stays quarantined; Release then
// delivers it. Repeating with the same owner succeeds; any other change, a
// resolved binding or a live release lease is refused.
func (s *Store) ResolveQuarantined(ctx context.Context, gateway, id string, b Binding) error {
	if b.Unresolved() || !identifier(b.Issuer) || !identifier(b.Subject) || !identifier(b.Mailbox) || b.Generation <= 0 {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var until int64
	err = tx.QueryRowContext(ctx, "SELECT state,lease_until FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&state, &until)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != "quarantined" || until > time.Now().Unix()) {
		return ErrNotQuarantined
	}
	if err != nil {
		return err
	}
	bindings, err := readBindings(ctx, tx, gateway, id)
	if err != nil {
		return err
	}
	if len(bindings) != 1 || bindings[0].Address != b.Address || !bindings[0].Unresolved() && bindings[0] != b {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "UPDATE bindings SET issuer=?,subject=?,mailbox=?,generation=? WHERE gateway=? AND id=?", b.Issuer, b.Subject, b.Mailbox, b.Generation, gateway, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListQuarantined pages quarantined envelopes across gateways, without MIME.
func (s *Store) ListQuarantined(ctx context.Context, after int64, limit int) ([]Quarantined, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT rowid,gateway,id,sender,created,length(raw) FROM deliveries WHERE state='quarantined' AND rowid>? ORDER BY rowid LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	var result []Quarantined
	for rows.Next() {
		var q Quarantined
		var created int64
		if err := rows.Scan(&q.Sequence, &q.Gateway, &q.ID, &q.Sender, &created, &q.Size); err != nil {
			rows.Close()
			return nil, err
		}
		q.Received = time.Unix(created, 0).UTC()
		result = append(result, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range result {
		if result[i].Bindings, err = readBindings(ctx, tx, result[i].Gateway, result[i].ID); err != nil {
			return nil, err
		}
	}
	return result, tx.Commit()
}
