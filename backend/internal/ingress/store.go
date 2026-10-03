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
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	_ "modernc.org/sqlite"
)

var (
	ErrConflict     = errors.New("ingress identity conflict; preserve the receipt and investigate")
	ErrCapacity     = errors.New("ingress capacity reached; import or archive receipts before accepting more mail")
	ErrRoute        = errors.New("recipient is unknown, disabled or routing is stale; reconcile the directory")
	ErrRoutingStale = errors.New("routing snapshot expired; refresh it before accepting mail")
	ErrLease        = errors.New("ingress claim expired or changed; reacquire before acknowledging")
)

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
}

type Summary struct {
	Sequence int64
	ID       string
	State    string
}

type Store struct {
	db     *sql.DB
	limits Limits
}

const schema = `
CREATE TABLE IF NOT EXISTS limits (id INTEGER PRIMARY KEY CHECK(id=1), message_bytes INTEGER, payload_bytes INTEGER, records INTEGER);
CREATE TABLE IF NOT EXISTS routes (address TEXT PRIMARY KEY, issuer TEXT NOT NULL, subject TEXT NOT NULL, mailbox TEXT NOT NULL, generation INTEGER NOT NULL, active INTEGER NOT NULL, valid_until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS deliveries (gateway TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, created INTEGER NOT NULL, digest TEXT NOT NULL DEFAULT '', raw BLOB, state TEXT NOT NULL DEFAULT 'staged', lease TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(gateway,id));
CREATE TABLE IF NOT EXISTS bindings (gateway TEXT NOT NULL, id TEXT NOT NULL, address TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL, mailbox TEXT NOT NULL, generation INTEGER NOT NULL, PRIMARY KEY(gateway,id,address), FOREIGN KEY(gateway,id) REFERENCES deliveries(gateway,id) ON DELETE CASCADE);
`

func Open(dir string, limits Limits) (*Store, error) {
	if limits.MessageBytes <= 0 || limits.MessageBytes > 64<<20 || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 {
		return nil, errors.New("invalid ingress limits")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
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
	db, err := sql.Open("sqlite", u.String()+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, limits: limits}
	tx, err := db.Begin()
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(schema)
		if err == nil {
			_, err = tx.Exec("INSERT OR IGNORE INTO limits VALUES(1,?,?,?)", limits.MessageBytes, limits.PayloadBytes, limits.Records)
		}
		var persisted Limits
		if err == nil {
			err = tx.QueryRow("SELECT message_bytes,payload_bytes,records FROM limits WHERE id=1").Scan(&persisted.MessageBytes, &persisted.PayloadBytes, &persisted.Records)
		}
		if err == nil && persisted != limits {
			err = errors.New("ingress limits differ from durable configuration")
		}
		if err == nil {
			err = tx.Commit()
		}
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

func identifier(v string) bool {
	return v != "" && len(v) <= 256 && !strings.ContainsAny(v, "\x00\r\n")
}

// SetRoute accepts only an already verified directory decision from a trusted
// writer. It does not verify a JWT or establish domain ownership.
func (s *Store) SetRoute(ctx context.Context, r Route) error {
	if !address(r.Address, false) || !identifier(r.Issuer) || !identifier(r.Subject) || !identifier(r.Mailbox) || r.Generation <= 0 || r.ValidUntil.IsZero() {
		return ErrRoute
	}
	tx, err := s.db.BeginTx(ctx, nil)
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

// Bind freezes a successful RCPT decision before returning to the receiver.
// deliveryID must be generated by the trusted receiver, never Message-ID.
func (s *Store) Bind(ctx context.Context, gateway, deliveryID, sender, recipient string) error {
	if !identifier(gateway) || !identifier(deliveryID) || !address(sender, true) || !address(recipient, false) {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldSender, oldDigest, state string
	err = tx.QueryRowContext(ctx, "SELECT sender,digest,state FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&oldSender, &oldDigest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
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
	var used int64
	// ponytail: scan at most limits.Records rows; add transactional byte counters
	// only if measured spool throughput makes this bounded scan a bottleneck.
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(raw)),0) FROM deliveries").Scan(&used); err != nil {
		return err
	}
	if int64(len(data)) > s.limits.PayloadBytes-used {
		return ErrCapacity
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET raw=?,digest=?,state='pending' WHERE gateway=? AND id=?", data, digest, gateway, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func readDelivery(ctx context.Context, tx *sql.Tx, gateway, id string) (Delivery, error) {
	d := Delivery{Gateway: gateway, ID: id}
	err := tx.QueryRowContext(ctx, "SELECT sender,digest,raw,state,lease FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&d.Sender, &d.Digest, &d.Raw, &d.State, &d.Lease)
	if err != nil {
		return d, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT address,issuer,subject,mailbox,generation FROM bindings WHERE gateway=? AND id=? ORDER BY address", gateway, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.Address, &b.Issuer, &b.Subject, &b.Mailbox, &b.Generation); err != nil {
			return d, err
		}
		d.Bindings = append(d.Bindings, b)
	}
	return d, rows.Err()
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

// List pages receipts, including staged/imported/quarantined records, without
// loading MIME. Sequence is an enumeration position, not a change-sync cursor;
// importers revisit unacknowledged receipts rather than advancing past them.
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
	if d.State != "pending" {
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
	if !current {
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
	if err != nil {
		return err
	}
	if oldDigest != digest || oldLease != lease {
		return ErrLease
	}
	if state == "imported" {
		return tx.Commit()
	}
	if state != "pending" || until <= time.Now().Unix() {
		return ErrLease
	}
	current, err := currentBindings(ctx, tx, gateway, id)
	if err != nil {
		return err
	}
	if !current {
		_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=?", gateway, id)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return ErrRoute
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='imported',raw=NULL WHERE gateway=? AND id=?", gateway, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
