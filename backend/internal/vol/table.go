package vol

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// Table keeps one root's documents in tenancy.db's documents table, PostgreSQL only. Every
// write takes a fresh revision from the table's one identity sequence, so no two versions of
// any document share one, before or after a remove: a writer that read before a remove and
// re-create can never match (ABA).
type Table struct {
	db   *sql.DB
	root string
}

var (
	errRoot    = errors.New("vol: the documents table needs PostgreSQL and a config or state root")
	errNoOwner = errors.New("vol: this path's owner mapping is not built yet (MH2)")
)

const (
	nextRev   = `nextval(pg_get_serial_sequence('documents', 'revision'))`
	nowMillis = `CAST(EXTRACT(EPOCH FROM statement_timestamp()) * 1000 AS BIGINT)`
)

// NewTable opens root ("config" or "state") over db, a tenancy.db handle.
func NewTable(db *sql.DB, root string) (*Table, error) {
	if db == nil || !dbbackend.Postgres() || root != "config" && root != "state" {
		return nil, errRoot
	}
	return &Table{db: db, root: root}, nil
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
	err := t.db.QueryRowContext(ctx, `SELECT body, revision FROM documents WHERE root = ? AND path = ?`, t.root, path).Scan(&body, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fs.ErrNotExist
	}
	return body, rev, err
}

func (t *Table) Write(ctx context.Context, path string, body []byte, rev int64) (int64, error) {
	if err := checkWrite(path, body, rev); err != nil {
		return 0, err
	}
	own, err := owner(path)
	if err != nil {
		return 0, err
	}
	if body == nil {
		body = []byte{}
	}
	q := `UPDATE documents SET body = ?, revision = ` + nextRev + `, updated_at = ` + nowMillis + `
 WHERE root = ? AND path = ? AND revision = ? RETURNING revision`
	args := []any{body, t.root, path, rev}
	if rev == 0 {
		// A create never meets a key violation: a racing creator just finds no row returned.
		q = `INSERT INTO documents (root, path, owner, body, updated_at) VALUES (?, ?, ?, ?, ` + nowMillis + `)
 ON CONFLICT (root, path) DO NOTHING RETURNING revision`
		args = []any{t.root, path, sql.NullString{String: own, Valid: own != ""}, body}
	}
	var next int64
	err = t.db.QueryRowContext(ctx, q, args...).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrStale
	}
	return next, err
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
