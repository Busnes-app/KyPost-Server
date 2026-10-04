// Package mailbox owns permanent raw mail and transactional delivery receipts.
// Runtime native access requires explicit mode and current ownership admission.
package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	_ "modernc.org/sqlite"
)

var (
	ErrOwner    = errors.New("mailbox owner differs from durable identity; reconcile provisioning")
	ErrConflict = errors.New("mailbox delivery receipt conflict; retain the holding copy and investigate")
	ErrCapacity = errors.New("mailbox capacity reached; preserve holding mail and review live-byte and retained-record limits before retry")
	ErrNotFound = errors.New("mailbox message or folder not found")
	ErrCursor   = errors.New("mailbox cursor outside durable history; perform a full resynchronization")
)

type Owner struct{ Issuer, Subject, Mailbox string }
type Limits struct {
	MessageBytes, PayloadBytes int64
	Records                    int
}

// Recipient records the frozen envelope binding, including its routing generation.
type Recipient struct {
	Address    string
	Generation int64
}
type Receipt struct {
	Gateway, Delivery, Sender string
	Recipients                []Recipient
}
type Message struct {
	ID                                          int64
	Folder, Sender, Subject, To, CC, BCC, AtUTC string
	Seen, Starred, Draft                        bool
	Labels                                      []string
}
type Change struct {
	Revision, ID int64
	Folder       string
	Removed      bool
}
type Store struct {
	db                  *sql.DB
	owner               Owner
	limits              Limits
	namespace           string
	referenceGeneration string
}

const schema = `
CREATE TABLE IF NOT EXISTS identity (id INTEGER PRIMARY KEY CHECK(id=1), issuer TEXT NOT NULL, subject TEXT NOT NULL, mailbox TEXT NOT NULL, message_bytes INTEGER NOT NULL, payload_bytes INTEGER NOT NULL, records INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS namespace (id INTEGER PRIMARY KEY CHECK(id=1), token TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS incoming_replacements (source_id INTEGER PRIMARY KEY REFERENCES messages(id), marker TEXT NOT NULL UNIQUE, source_hash TEXT NOT NULL, ciphertext_hash TEXT NOT NULL, replacement_id INTEGER NOT NULL UNIQUE REFERENCES messages(id));
CREATE TABLE IF NOT EXISTS folders (name TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS labels (name TEXT PRIMARY KEY COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY AUTOINCREMENT, folder TEXT NOT NULL REFERENCES folders(name), raw BLOB, digest TEXT NOT NULL, sender TEXT NOT NULL, subject TEXT NOT NULL, sent_to TEXT NOT NULL, cc TEXT NOT NULL, bcc TEXT NOT NULL, at_utc TEXT NOT NULL, seen INTEGER NOT NULL DEFAULT 0, starred INTEGER NOT NULL DEFAULT 0, draft INTEGER NOT NULL DEFAULT 0, labels TEXT NOT NULL DEFAULT '[]');
CREATE INDEX IF NOT EXISTS message_folder_ids ON messages(folder,id) WHERE raw IS NOT NULL;
CREATE TABLE IF NOT EXISTS receipts (gateway TEXT NOT NULL, delivery TEXT NOT NULL, envelope TEXT NOT NULL, digest TEXT NOT NULL, message_id INTEGER NOT NULL REFERENCES messages(id), PRIMARY KEY(gateway,delivery));
CREATE TABLE IF NOT EXISTS changes (revision INTEGER PRIMARY KEY AUTOINCREMENT, message_id INTEGER NOT NULL, folder TEXT NOT NULL, removed INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS usage (id INTEGER PRIMARY KEY CHECK(id=1), records INTEGER NOT NULL, payload_bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS outbox (id TEXT PRIMARY KEY, ciphertext BLOB NOT NULL, sent_bytes INTEGER NOT NULL, sent_reserved INTEGER NOT NULL, sent_id INTEGER REFERENCES messages(id), created_at INTEGER NOT NULL DEFAULT (unixepoch()));
CREATE TABLE IF NOT EXISTS outbox_deliveries (job TEXT NOT NULL REFERENCES outbox(id), sequence INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','submitting','accepted','retryable','failed','uncertain','quarantined')), claim TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job,sequence));
CREATE TRIGGER IF NOT EXISTS outbox_insert AFTER INSERT ON outbox BEGIN UPDATE usage SET records=records+1+NEW.sent_reserved,payload_bytes=payload_bytes+length(NEW.ciphertext)+NEW.sent_bytes WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS outbox_update AFTER UPDATE ON outbox BEGIN UPDATE usage SET records=records+NEW.sent_reserved-OLD.sent_reserved,payload_bytes=payload_bytes+length(NEW.ciphertext)-length(OLD.ciphertext)+NEW.sent_bytes-OLD.sent_bytes WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS outbox_delivery_insert AFTER INSERT ON outbox_deliveries BEGIN UPDATE usage SET records=records+1 WHERE id=1; END;

CREATE TRIGGER IF NOT EXISTS mailbox_insert AFTER INSERT ON messages BEGIN UPDATE usage SET records=records+1,payload_bytes=payload_bytes+coalesce(length(NEW.raw),0) WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS mailbox_raw_update AFTER UPDATE OF raw ON messages BEGIN UPDATE usage SET payload_bytes=payload_bytes-coalesce(length(OLD.raw),0)+coalesce(length(NEW.raw),0) WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS mailbox_delete AFTER DELETE ON messages BEGIN UPDATE usage SET records=records-1,payload_bytes=payload_bytes-coalesce(length(OLD.raw),0) WHERE id=1; END;
`

func Open(dir string, owner Owner, limits Limits) (*Store, error) {
	return open(dir, owner, limits, "")
}

// OpenExisting refuses missing or differently bound databases before migrations.
// Runtime callers supply the acknowledged source, never a guessed namespace.
func OpenExisting(dir string, owner Owner, limits Limits, source string) (*Store, error) {
	if !strings.HasPrefix(source, "native:") {
		return nil, ErrPreparation
	}
	return open(dir, owner, limits, source)
}

func open(dir string, owner Owner, limits Limits, source string) (*Store, error) {
	if !validText(owner.Issuer, 2048) || !validText(owner.Subject, 512) || !fsutil.SafePathComponent(owner.Mailbox) || limits.MessageBytes <= 0 || limits.MessageBytes > mailmsg.MaxInboundMessageBytes || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 {
		return nil, errors.New("invalid mailbox identity or limits")
	}
	if source == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("mailbox directory must be owner-only")
	}
	path, err := filepath.Abs(filepath.Join(dir, "mailbox.db"))
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	query := "?_txlock=immediate&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if source != "" {
		query += "&mode=rw"
	} else {
		query += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", u.String()+query)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, owner: owner, limits: limits}
	if source != "" {
		var storedOwner Owner
		var storedLimits Limits
		err = db.QueryRow("SELECT issuer,subject,mailbox,message_bytes,payload_bytes,records FROM identity WHERE id=1").Scan(&storedOwner.Issuer, &storedOwner.Subject, &storedOwner.Mailbox, &storedLimits.MessageBytes, &storedLimits.PayloadBytes, &storedLimits.Records)
		if err == nil {
			err = db.QueryRow("SELECT token FROM namespace WHERE id=1").Scan(&s.namespace)
		}
		if err != nil || storedOwner != owner || storedLimits != limits || s.namespace == "" || (&Client{store: s}).MailSourceIdentity() != source {
			_ = db.Close()
			return nil, ErrPreparation
		}
		if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := fsutil.SyncDir(dir); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO identity VALUES(1,?,?,?,?,?,?)", s.owner.Issuer, s.owner.Subject, s.owner.Mailbox, s.limits.MessageBytes, s.limits.PayloadBytes, s.limits.Records); err != nil {
		return err
	}
	var initialized int
	if err = tx.QueryRow("SELECT count(*) FROM usage WHERE id=1").Scan(&initialized); err != nil {
		return err
	}
	if initialized == 0 {
		if _, err = tx.Exec(`INSERT INTO usage SELECT 1,
 (SELECT count(*) FROM messages)+(SELECT count(*)+coalesce(sum(sent_reserved),0) FROM outbox)+(SELECT count(*) FROM outbox_deliveries),
 (SELECT coalesce(sum(length(raw)),0) FROM messages)+(SELECT coalesce(sum(length(ciphertext)+sent_bytes),0) FROM outbox)`); err != nil {
			return err
		}
	}
	var owner Owner
	var limits Limits
	if err = tx.QueryRow("SELECT issuer,subject,mailbox,message_bytes,payload_bytes,records FROM identity WHERE id=1").Scan(&owner.Issuer, &owner.Subject, &owner.Mailbox, &limits.MessageBytes, &limits.PayloadBytes, &limits.Records); err != nil {
		return err
	}
	if owner != s.owner {
		return ErrOwner
	}
	if limits != s.limits {
		return errors.New("mailbox limits differ from durable configuration")
	}
	var namespaces int
	if err = tx.QueryRow("SELECT count(*) FROM namespace").Scan(&namespaces); err != nil {
		return err
	}
	if namespaces == 0 {
		token, e := fsutil.NewUUIDv4()
		if e != nil {
			return e
		}
		if _, err = tx.Exec("INSERT INTO namespace VALUES(1,?)", token); err != nil {
			return err
		}
	}
	if err = tx.QueryRow("SELECT token FROM namespace WHERE id=1").Scan(&s.namespace); err != nil {
		return err
	}
	if s.referenceGeneration, err = messageReferenceGeneration(tx); err != nil {
		return err
	}
	if s.referenceGeneration == "" {
		s.referenceGeneration, err = fsutil.NewUUIDv4()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(referenceGenerationSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reference_generation VALUES(1,?)", s.referenceGeneration); err != nil {
			return err
		}
	}
	for _, folder := range []string{"INBOX", "Drafts", "Sent", "Trash", "Junk", "Archive"} {
		if _, err = tx.Exec("INSERT OR IGNORE INTO folders VALUES(?)", folder); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Owner() Owner { return s.owner }

func validText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
func validFolder(folder string) bool {
	if !validText(folder, 255) {
		return false
	}
	for _, part := range strings.Split(folder, "/") {
		if strings.TrimSpace(part) != part || part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func canonicalReceipt(r Receipt) (string, error) {
	if !validText(r.Gateway, 128) || !validText(r.Delivery, 256) || len(r.Recipients) == 0 || len(r.Recipients) > 100 {
		return "", errors.New("invalid mailbox delivery receipt")
	}
	if r.Sender != "" {
		a, err := mail.ParseAddress(r.Sender)
		if err != nil || a.Address != r.Sender {
			return "", errors.New("invalid envelope sender")
		}
	}
	// Copy before sorting: the caller's frozen binding list remains untouched.
	r.Recipients = append([]Recipient(nil), r.Recipients...)
	sort.Slice(r.Recipients, func(i, j int) bool { return r.Recipients[i].Address < r.Recipients[j].Address })
	for i, p := range r.Recipients {
		a, err := mail.ParseAddress(p.Address)
		if err != nil || a.Address != p.Address || p.Generation <= 0 || (i > 0 && r.Recipients[i-1].Address == p.Address) {
			return "", errors.New("invalid frozen recipient")
		}
	}
	data, err := json.Marshal(r)
	return string(data), err
}

// Import atomically commits exact bytes, metadata, change and immutable receipt.
// An exact replay returns the original ID even after the user moved/deleted mail.
func (s *Store) Import(ctx context.Context, receipt Receipt, input io.Reader) (int64, error) {
	envelope, err := canonicalReceipt(receipt)
	if err != nil {
		return 0, err
	}
	return s.append(ctx, "INBOX", input, receipt.Gateway, receipt.Delivery, envelope, false)
}

// Append stores a local Drafts/Sent copy without an SMTP delivery receipt.
func (s *Store) Append(ctx context.Context, folder string, input io.Reader, draft bool) (int64, error) {
	if !validFolder(folder) {
		return 0, errors.New("invalid folder")
	}
	return s.append(ctx, folder, input, "", "", "", draft)
}
func (s *Store) append(ctx context.Context, folder string, input io.Reader, gateway, delivery, envelope string, draft bool) (int64, error) {
	raw, err := mailmsg.BoundedRead(input, s.limits.MessageBytes)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if gateway != "" {
		var oldEnvelope, oldDigest string
		var id int64
		err = tx.QueryRowContext(ctx, "SELECT envelope,digest,message_id FROM receipts WHERE gateway=? AND delivery=?", gateway, delivery).Scan(&oldEnvelope, &oldDigest, &id)
		if err == nil {
			if oldEnvelope != envelope || oldDigest != digest {
				return 0, ErrConflict
			}
			return id, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	var count, used int64
	// SQLite triggers maintain counters in the message transaction across writers.
	if err = tx.QueryRowContext(ctx, "SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&count, &used); err != nil {
		return 0, err
	}
	// Tombstones/receipts consume the record budget too; never silently evict identity history.
	if count >= int64(s.limits.Records) || int64(len(raw)) > s.limits.PayloadBytes-used {
		return 0, ErrCapacity
	}
	headers, at, err := rawMetadata(raw)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(folder,raw,digest,sender,subject,sent_to,cc,bcc,at_utc,seen,draft) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, folder, raw, digest, headers.Get("From"), headers.Get("Subject"), headers.Get("To"), headers.Get("Cc"), headers.Get("Bcc"), at.Format(time.RFC3339), folder != "INBOX" && !draft, draft)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if gateway != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO receipts VALUES(?,?,?,?,?)", gateway, delivery, envelope, digest, id); err != nil {
			return 0, err
		}
	}
	if err = change(ctx, tx, id, folder, false); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func change(ctx context.Context, tx *sql.Tx, id int64, folder string, removed bool) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO changes(message_id,folder,removed) VALUES(?,?,?)", id, folder, removed)
	return err
}
func (s *Store) Raw(ctx context.Context, folder string, id int64) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT raw FROM messages WHERE folder=? AND id=? AND raw IS NOT NULL", folder, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

// List is keyset pagination of live metadata, never the bounded/rebuildable mail cache.
// before=0 starts at newest. Concurrent mutations are reconciled using Changes.
func (s *Store) List(ctx context.Context, folder string, before int64, limit int) ([]Message, error) {
	if !validFolder(folder) || before < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid mailbox page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,folder,sender,subject,sent_to,cc,bcc,at_utc,seen,starred,draft,labels FROM messages WHERE folder=? AND raw IS NOT NULL AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, folder, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Message{}
	for rows.Next() {
		var m Message
		var labels string
		if err = rows.Scan(&m.ID, &m.Folder, &m.Sender, &m.Subject, &m.To, &m.CC, &m.BCC, &m.AtUTC, &m.Seen, &m.Starred, &m.Draft, &labels); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(labels), &m.Labels); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// Changes returns bounded events and a high-water mark from one snapshot.
// Advance only to the last returned event; when empty, advance to highWater.
// History is retained in full; future/out-of-range cursors fail explicitly.
func (s *Store) Changes(ctx context.Context, after int64, limit int) ([]Change, int64, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, 0, ErrCursor
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var high int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(revision),0) FROM changes").Scan(&high); err != nil {
		return nil, 0, err
	}
	if after > high {
		return nil, high, ErrCursor
	}
	rows, err := tx.QueryContext(ctx, "SELECT revision,message_id,folder,removed FROM changes WHERE revision>? ORDER BY revision LIMIT ?", after, limit)
	if err != nil {
		return nil, 0, err
	}
	result := []Change{}
	for rows.Next() {
		var c Change
		if err = rows.Scan(&c.Revision, &c.ID, &c.Folder, &c.Removed); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		result = append(result, c)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	return result, high, tx.Commit()
}

// Update scopes flags/labels to the current folder and ID. Identical retries are no-ops.
func (s *Store) Update(ctx context.Context, folder string, id int64, seen, starred bool, labels []string) error {
	if len(labels) > 100 {
		return errors.New("too many mailbox labels")
	}
	labels = append([]string{}, labels...)
	sort.Strings(labels)
	seenLabels := map[string]bool{}
	for _, label := range labels {
		if !validText(label, 128) || strings.TrimSpace(label) != label || seenLabels[strings.ToLower(label)] {
			return errors.New("invalid mailbox label")
		}
		seenLabels[strings.ToLower(label)] = true
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldSeen, oldStarred bool
	var oldLabels string
	err = tx.QueryRowContext(ctx, "SELECT seen,starred,labels FROM messages WHERE folder=? AND id=? AND raw IS NOT NULL", folder, id).Scan(&oldSeen, &oldStarred, &oldLabels)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	for _, label := range labels {
		if err = ensureLabelTx(ctx, tx, label); err != nil {
			return err
		}
	}
	if oldSeen == seen && oldStarred == starred && oldLabels == string(data) {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET seen=?,starred=?,labels=? WHERE id=?", seen, starred, string(data), id); err != nil {
		return err
	}
	if err = change(ctx, tx, id, folder, false); err != nil {
		return err
	}
	return tx.Commit()
}

// Move preserves the numeric ID and emits a removal and arrival atomically.
// Delete permanently clears raw bytes while retaining the ID/receipt/tombstone.
func (s *Store) Move(ctx context.Context, folder string, id int64, target string) error {
	if !validFolder(target) {
		return errors.New("invalid target folder")
	}
	return s.relocate(ctx, folder, id, target)
}
func (s *Store) Delete(ctx context.Context, folder string, id int64) error {
	return s.relocate(ctx, folder, id, "")
}
func (s *Store) relocate(ctx context.Context, folder string, id int64, target string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	err = tx.QueryRowContext(ctx, "SELECT folder FROM messages WHERE id=? AND folder=? AND raw IS NOT NULL", id, folder).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if target == folder {
		return tx.Commit()
	}
	if target == "" {
		_, err = tx.ExecContext(ctx, "UPDATE messages SET raw=NULL,sender='',subject='',sent_to='',cc='',bcc='',labels='[]' WHERE id=?", id)
	} else {
		var exists string
		err = tx.QueryRowContext(ctx, "SELECT name FROM folders WHERE name=?", target).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE messages SET folder=? WHERE id=?", target, id)
		}
	}
	if err != nil {
		return err
	}
	if err = change(ctx, tx, id, folder, true); err != nil {
		return err
	}
	if target != "" {
		if err = change(ctx, tx, id, target, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Folders(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM folders ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}
func (s *Store) CreateFolder(ctx context.Context, name string) error {
	if !validFolder(name) {
		return errors.New("invalid folder")
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO folders VALUES(?)", name)
	return err
}

// DeleteFolder refuses live mail and children; deleting folders never discards mail.
func (s *Store) DeleteFolder(ctx context.Context, name string) error {
	for _, special := range []string{"INBOX", "Drafts", "Sent", "Trash", "Junk", "Archive"} {
		if name == special {
			return errors.New("cannot delete system folder")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE folder=? AND raw IS NOT NULL", name).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("folder contains mail; move it before deletion")
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE substr(name,1,length(?)+1)=?||'/'", name, name).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("folder contains children; remove them first")
	}
	// Retained message identities no longer need the removed folder.
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET folder='Trash' WHERE folder=? AND raw IS NULL", name); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, "DELETE FROM folders WHERE name=?", name)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// RenameFolder moves a whole subtree and its messages in one transaction.
// Existing destination names and renaming a system folder are refused.
func (s *Store) RenameFolder(ctx context.Context, folder, target string) error {
	if !validFolder(folder) || !validFolder(target) || folder == target || strings.HasPrefix(target, folder+"/") {
		return errors.New("invalid folder rename")
	}
	for _, special := range []string{"INBOX", "Drafts", "Sent", "Trash", "Junk", "Archive"} {
		if folder == special {
			return errors.New("cannot rename system folder")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, "SELECT name FROM folders WHERE name=? OR substr(name,1,length(?)+1)=?||'/' ORDER BY name", folder, folder, folder)
	if err != nil {
		return err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(names) == 0 || names[0] != folder {
		return ErrNotFound
	}
	for _, name := range names {
		newName := target + strings.TrimPrefix(name, folder)
		if !validFolder(newName) {
			return errors.New("renamed folder path exceeds limit")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO folders VALUES(?)", newName); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT id FROM messages WHERE folder=? AND raw IS NOT NULL ORDER BY id", name)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr = rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if _, err = tx.ExecContext(ctx, "UPDATE messages SET folder=? WHERE folder=?", newName, name); err != nil {
			return err
		}
		for _, id := range ids {
			if err = change(ctx, tx, id, name, true); err != nil {
				return err
			}
			if err = change(ctx, tx, id, newName, false); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM folders WHERE name=?", name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func rawMetadata(raw []byte) (mail.Header, time.Time, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, time.Time{}, errors.New("invalid RFC5322 message; retain holding copy")
	}
	// Bound persisted header metadata; bodies/PGP payloads stay opaque in the raw BLOB.
	for _, field := range []string{"From", "Subject", "To", "Cc", "Bcc", "Date"} {
		if len(m.Header.Get(field)) > 64<<10 {
			return nil, time.Time{}, errors.New("mailbox header exceeds metadata limit")
		}
	}
	at := time.Now().UTC()
	if date, e := m.Header.Date(); e == nil {
		at = date.UTC()
	}
	return m.Header, at, nil
}
