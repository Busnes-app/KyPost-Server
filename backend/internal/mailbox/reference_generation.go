package mailbox

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

const referenceGenerationSchema = `CREATE TABLE reference_generation (id INTEGER PRIMARY KEY CHECK(id=1), token TEXT NOT NULL)`

// Shared by writable initialization and read-only preparation validation, on a SQLite
// handle.
func messageReferenceGeneration(q interface{ QueryRow(string, ...any) *sql.Row }) (string, error) {
	return referenceGenerationOn(q, false)
}

// pg says q is a Postgres schema, where the table always exists and an empty one is an
// older mailbox's absence.
func referenceGenerationOn(q interface{ QueryRow(string, ...any) *sql.Row }, pg bool) (string, error) {
	var exists int
	var kind string
	query := "SELECT count(*),coalesce(min(type),'') FROM sqlite_schema WHERE name='reference_generation'"
	if pg {
		query = "SELECT count(*),'table' FROM (SELECT 1 FROM reference_generation LIMIT 1) r"
	}
	if err := q.QueryRow(query).Scan(&exists, &kind); err != nil {
		return "", err
	}
	if exists == 0 {
		return "", nil // Older mailboxes have no generation metadata.
	}
	if exists != 1 || kind != "table" {
		return "", ErrPreparation
	}
	var count int
	var token string
	if err := q.QueryRow("SELECT count(*),coalesce(min(token),'') FROM reference_generation").Scan(&count, &token); err != nil {
		return "", err
	}
	if count != 1 || len(token) != 36 || token[8] != '-' || token[13] != '-' || token[18] != '-' || token[23] != '-' || token[14] != '4' || !strings.ContainsRune("89ab", rune(token[19])) || strings.ToLower(token) != token {
		return "", ErrPreparation
	}
	if decoded, err := hex.DecodeString(strings.ReplaceAll(token, "-", "")); err != nil || len(decoded) != 16 {
		return "", ErrPreparation
	}
	var id int
	if err := q.QueryRow("SELECT id FROM reference_generation").Scan(&id); err != nil || id != 1 {
		return "", ErrPreparation
	}
	return token, nil
}

// MessageReferenceGeneration is stable for a store lifetime and ordinary reopen.
// Native HTTP and notification references use this generation; internal IDs stay numeric.
func (s *Store) MessageReferenceGeneration() string { return s.referenceGeneration }

// RotateRestoredMessageReferences requires stopped, private restore staging that
// has passed whole-snapshot validation. Never call against a running mailbox.
// Bind to the acknowledged source before mutation; preserve namespace and mail.
func RotateRestoredMessageReferences(path, source string) (err error) {
	if err := dbbackend.SQLiteOnly(path); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open("sqlite", u.String()+"?mode=rw&_txlock=immediate&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	s := &Store{}
	if err = tx.QueryRow("SELECT issuer,subject,mailbox FROM identity WHERE id=1").Scan(&s.owner.Issuer, &s.owner.Subject, &s.owner.Mailbox); err != nil {
		return err
	}
	if err = tx.QueryRow("SELECT token FROM namespace WHERE id=1").Scan(&s.namespace); err != nil {
		return err
	}
	if !strings.HasPrefix(source, "native:") || s.namespace == "" || (&Client{store: s}).MailSourceIdentity() != source {
		return ErrPreparation
	}
	prior, err := messageReferenceGeneration(tx)
	if err != nil {
		return err
	}
	if prior == "" {
		if _, err = tx.Exec(referenceGenerationSchema); err != nil {
			return err
		}
	}
	token, err := fsutil.NewUUIDv4()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO reference_generation VALUES(1,?) ON CONFLICT(id) DO UPDATE SET token=excluded.token", token); err != nil {
		return err
	}
	return tx.Commit()
}

// InspectRestored reads a mailbox read-only: its reference generation ("" for
// older databases) and how many outbound deliveries are queued or retryable.
//
// On Postgres (dbbackend.Held) it reads the schema; a configuration or catalog error is
// returned, never answered from a file.
func InspectRestored(path string) (generation string, pending int, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", 0, err
	}
	pg, err := dbbackend.Held(absolute)
	if err != nil {
		return "", 0, err
	}
	var db *sql.DB
	if pg {
		if db, err = dbbackend.Open(absolute, dbbackend.Options{Kind: Kind, ReadOnly: true}); err != nil {
			return "", 0, err
		}
	} else {
		u := url.URL{Scheme: "file", Path: absolute}
		if db, err = sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)"); err != nil {
			return "", 0, err
		}
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if generation, err = referenceGenerationOn(db, pg); err != nil {
		return "", 0, err
	}
	var tables int
	if err = db.QueryRow(pick(pg, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('outbox','outbox_deliveries')",
		"SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND tablename IN ('outbox','outbox_deliveries')")).Scan(&tables); err != nil || tables == 0 {
		return generation, 0, err
	}
	if tables != 2 {
		return "", 0, ErrOutbound
	}
	err = db.QueryRow("SELECT count(*) FROM outbox_deliveries WHERE state IN ('queued','retryable')").Scan(&pending)
	return generation, pending, err
}
