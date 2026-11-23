package vol

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

// Table keeps one root's documents in volumes.db's documents table, PostgreSQL only. Every
// write takes a fresh revision from the table's one identity sequence, so no two versions of
// any document share one, before or after a remove: a writer that read before a remove and
// re-create can never match (ABA).
type Table struct {
	db       *sql.DB
	snapshot bool
	root     string
	master   []byte
	ownerFor func(context.Context, string, []byte) (string, error)
	orgFor   func(context.Context, string) (string, error)
}

// TableOptions supplies trusted ownership adapters and the provisioned documents.key.
// Owner maps mailbox/pickup paths from the ledger or record. User paths map themselves.
// Org resolves a user from retained local identity, returning empty for a migrated single install.
type TableOptions struct {
	MasterKey []byte
	Owner     func(context.Context, string, []byte) (string, error)
	Org       func(context.Context, string) (string, error)
}

var (
	errRoot    = errors.New("vol: the documents table needs PostgreSQL and a config or state root")
	errNoOwner = errors.New("vol: this path's owner mapping is not built yet (MH2)")
)

const (
	nextRev   = `nextval(pg_get_serial_sequence('documents', 'revision'))`
	nowMillis = `CAST(EXTRACT(EPOCH FROM statement_timestamp()) * 1000 AS BIGINT)`
)

// NewTable opens root ("config" or "state") over a volumes.db handle. It never
// creates a master key or permits a table backed by SQLite at runtime.
func NewTable(db *sql.DB, root string, options TableOptions) (*Table, error) {
	if db == nil || root != "config" && root != "state" {
		return nil, errRoot
	}
	if _, ok := db.Driver().(pgdialect.Driver); !ok {
		return nil, errRoot
	}
	if len(options.MasterKey) != 32 {
		return nil, errors.New("vol: provisioned documents.key is missing or invalid")
	}
	if options.Org == nil {
		return nil, errors.New("vol: document org resolver is required")
	}
	return &Table{db: db, root: root, master: append([]byte(nil), options.MasterKey...), ownerFor: options.Owner, orgFor: options.Org}, nil
}

// owner is a document's owning user for a D row, "" for a P row. It comes from the path,
// never from a request. Mailbox and pickup documents need the ledger's and the record's
// owner (MH2), so until then they are refused rather than stored without one.
func owner(path string) (string, error) {
	first, rest, _ := strings.Cut(path, "/")
	switch first {
	case "users":
		id, _, _ := strings.Cut(rest, "/")
		if id == "" {
			return "", ErrPath
		}
		return id, nil
	case "mailboxes", "pickup":
		return "", errNoOwner
	}
	return "", nil
}

func (t *Table) Read(ctx context.Context, path string) ([]byte, int64, error) {
	if err := CheckPath(path); err != nil {
		return nil, 0, err
	}
	var body []byte
	var rev int64
	var own sql.NullString
	err := t.db.QueryRowContext(ctx, `SELECT body, revision, owner FROM documents WHERE root = ? AND path = ?`, t.root, path).Scan(&body, &rev, &own)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fs.ErrNotExist
	}
	if err != nil {
		return nil, 0, err
	}
	// A user path cannot be downgraded into an unsealed platform document by changing metadata.
	expected, ownerErr := owner(path)
	if ownerErr == nil && expected != own.String || errors.Is(ownerErr, errNoOwner) && !own.Valid {
		return nil, 0, errOwner
	}
	if own.Valid {
		org, err := t.orgFor(ctx, own.String)
		if err != nil {
			return nil, 0, err
		}
		key, err := t.dataKey(ctx, t.db, org, false)
		if err != nil {
			return nil, 0, err
		}
		body, err = openDocument(body, key, t.binding(path, own.String, org))
		if err != nil {
			return nil, 0, err
		}
	}
	return body, rev, nil
}

var errOwner = errors.New("vol: document ownership mismatch")
var errSealed = errors.New("vol: document authentication failed")

// Write compares the revision before creating a key. The data key and sealed document
// commit together. MH3 adds the accounts-row guard before either insert.
func (t *Table) Write(ctx context.Context, path string, body []byte, rev int64) (int64, error) {
	return t.write(ctx, path, body, rev, 0)
}

func (t *Table) write(ctx context.Context, path string, body []byte, rev, limit int64) (int64, error) {
	if err := checkWrite(path, body, rev); err != nil {
		return 0, err
	}
	own, err := owner(path)
	if err != nil && errors.Is(err, errNoOwner) && t.ownerFor != nil {
		own, err = t.ownerFor(ctx, path, body)
	}
	if err != nil {
		return 0, err
	}
	if (strings.HasPrefix(path, "mailboxes/") || strings.HasPrefix(path, "pickup/")) && own == "" {
		return 0, errOwner
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// Rollback is cleanup on early return; after Commit it reports ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	var current int64
	var heldOwner sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT revision, owner FROM documents WHERE root = ? AND path = ?`, t.root, path).Scan(&current, &heldOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if current != rev {
		return 0, ErrStale
	}
	if current != 0 && own != heldOwner.String {
		return 0, errOwner
	}
	if limit > 0 && current == 0 {
		used, err := t.directoryBytes(ctx, tx, filepath.ToSlash(filepath.Dir(path)))
		if err != nil {
			return 0, err
		}
		if int64(len(body)) > limit-used {
			return 0, ErrDirectoryFull
		}
	}
	if own != "" {
		org, err := t.orgFor(ctx, own)
		if err != nil {
			return 0, err
		}
		key, err := t.dataKey(ctx, tx, org, true)
		if err != nil {
			return 0, err
		}
		body, err = sealDocument(body, key, t.binding(path, own, org))
		if err != nil {
			return 0, err
		}
	}
	if body == nil {
		body = []byte{}
	}
	next, now := t.expressions()
	q := `UPDATE documents SET body = ?, revision = ` + next + `, updated_at = ` + now + `
 WHERE root = ? AND path = ? AND revision = ? RETURNING revision`
	args := []any{body, t.root, path, rev}
	if rev == 0 {
		q = `INSERT INTO documents (root, path, owner, body, updated_at) VALUES (?, ?, ?, ?, ` + now + `)
 ON CONFLICT (root, path) DO NOTHING RETURNING revision`
		args = []any{t.root, path, sql.NullString{String: own, Valid: own != ""}, body}
		if t.snapshot {
			q = `INSERT INTO documents (root,path,owner,body,revision,updated_at) VALUES(?,?,?,?,` + next + `,` + now + `) ON CONFLICT(root,path) DO NOTHING RETURNING revision`
		}
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrStale
		}
		return 0, err
	}
	return revision, tx.Commit()
}

// Length-prefixed JSON strings prevent ambiguous root/path/user boundaries. The org is
// included too, binding ciphertext even if a wrongly copied key were identical.
func (t *Table) binding(path, own, org string) []byte {
	b, _ := json.Marshal([]string{"kypost:document:1", t.root, path, own, org})
	return b
}

type keyReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (t *Table) dataKey(ctx context.Context, db keyReader, org string, create bool) ([]byte, error) {
	aad, _ := json.Marshal([]string{"kypost:document-key:1", org})
	var wrapped []byte
	err := db.QueryRowContext(ctx, `SELECT wrapped FROM doc_keys WHERE org_id = ?`, org).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		wrapped, err = sealDocument(key, t.master, aad)
		if err != nil {
			return nil, err
		}
		// Only Write, inside its transaction, may create a data key.
		_, now := t.expressions()
		if _, err = db.ExecContext(ctx, `INSERT INTO doc_keys(org_id, wrapped, created_at) VALUES(?, ?, `+now+`) ON CONFLICT DO NOTHING`, org, wrapped); err != nil {
			return nil, err
		}
		err = db.QueryRowContext(ctx, `SELECT wrapped FROM doc_keys WHERE org_id = ?`, org).Scan(&wrapped)
	}
	if err != nil {
		return nil, errSealed
	}
	key, err := openDocument(wrapped, t.master, aad)
	if err != nil || len(key) != 32 {
		return nil, errSealed
	}
	return key, nil
}

func sealDocument(body, key, aad []byte) ([]byte, error) {
	env, err := cryptutil.SealAssociated(body, key, aad)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func openDocument(body, key, aad []byte) ([]byte, error) {
	env, ok := cryptutil.ParseEnvelope(body)
	if !ok {
		return nil, errSealed
	}
	plain, err := cryptutil.OpenAssociated(env, key, aad)
	if err != nil {
		return nil, errSealed
	}
	return plain, nil
}

func (t *Table) Remove(ctx context.Context, path string, rev int64) error {
	if err := CheckPath(path); err != nil {
		return err
	}
	if rev <= 0 {
		return ErrStale
	}
	res, err := t.db.ExecContext(ctx, `DELETE FROM documents WHERE root = ? AND path = ? AND revision = ?`, t.root, path, rev)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		err = ErrStale
	}
	return err
}

// List matches paths strictly under prefix + "/" by range, so no LIKE wildcard in a path
// widens it ('0' is the byte after '/'; the database is C-collated, dbbackend.Check).
func (t *Table) List(ctx context.Context, prefix string) ([]string, error) {
	if err := CheckPath(prefix); err != nil {
		return nil, err
	}
	rows, err := t.db.QueryContext(ctx, `SELECT path FROM documents WHERE root = ? AND path > ? AND path < ? ORDER BY path`,
		t.root, prefix+"/", prefix+"0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("vol: list: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NewSnapshotTable is used only while operating on stopped, private SQLite restore
// staging. Runtime opens continue to require PostgreSQL through NewTable.
func NewSnapshotTable(db *sql.DB, root string, options TableOptions) (*Table, error) {
	if db == nil || root != "config" && root != "state" || options.Org == nil {
		return nil, errRoot
	}
	var version string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&version); err != nil {
		return nil, errRoot
	}
	return &Table{db: db, root: root, master: append([]byte(nil), options.MasterKey...), ownerFor: options.Owner, orgFor: options.Org, snapshot: true}, nil
}

func (t *Table) expressions() (revision, now string) {
	if t.snapshot {
		// Restore staging has one offline writer. ImportSQLite starts the live identity
		// sequence above every restored row before any replica can run.
		return `(SELECT coalesce(max(revision),0)+1 FROM documents)`, `CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)`
	}
	return nextRev, nowMillis
}
