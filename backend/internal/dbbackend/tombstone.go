package dbbackend

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

// Directory markers live in the existing catalog, with a trailing slash so a
// sibling prefix never matches. They are not openable stores or backup members.
const purgedKind = "purged"
const tombstoneQuery = `SELECT EXISTS(SELECT 1 FROM kypost_catalog.stores WHERE kind='purged' AND path=ANY(?))`

// Probe only indexed ancestors, rather than scanning every retained directory
// marker on every store open. Catalog paths use slash separators on every host.
func directoryAncestors(path string) []string {
	var out []string
	for slash := strings.LastIndexByte(path, '/'); slash >= 0; slash = strings.LastIndexByte(path, '/') {
		out = append(out, path[:slash+1])
		path = path[:slash]
	}
	return out
}

// ErrPurged is a missing store whose directory was permanently purged.
var ErrPurged = fmt.Errorf("database directory was purged: %w", ErrNotFound)

func refusePurged(row *sql.Row) error {
	var gone bool
	if err := row.Scan(&gone); err != nil {
		return err
	}
	if gone {
		return ErrPurged
	}
	return nil
}

func directoryPrefix(path string) (string, error) {
	_, rel, err := SchemaName(filepath.Join(path, "x.db"))
	return strings.TrimSuffix(rel, "x.db"), err
}

// TombstoneDir atomically drops every schema under path and records a permanent
// directory marker, including when no schema has been published there yet. It
// is selected only by table-mode purge; ordinary directory removal is unchanged.
func TombstoneDir(ctx context.Context, path string) error {
	cat, err := root(ctx)
	if err != nil {
		return err
	}
	prefix, err := directoryPrefix(path)
	if err != nil {
		return err
	}
	markerSchema, _, err := SchemaName(path)
	if err != nil {
		return err
	}
	tx, err := cat.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT schema FROM kypost_catalog.stores WHERE left(path,length(?))=? AND kind<>? ORDER BY path`, prefix, prefix, purgedKind)
	if err != nil {
		return err
	}
	var schemas []string
	for rows.Next() {
		var schema string
		if err = rows.Scan(&schema); err != nil {
			rows.Close()
			return err
		}
		schemas = append(schemas, schema)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, schema := range schemas {
		// Keep the existing writer key (search_path includes public for citext).
		// A writer already holding it finishes before deletion; a stale waiter
		// acquires it only after deletion and refuses the missing namespace.
		writer, _ := pgdialect.LockKeys(schema + ",public")
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, writer); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+QuoteIdent(schema)+` CASCADE`); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM kypost_catalog.stores WHERE left(path,length(?))=? AND kind<>?`, prefix, prefix, purgedKind); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO kypost_catalog.stores(path,schema,kind,ddl) VALUES(?,?,?,'') ON CONFLICT(path) DO NOTHING`, prefix, markerSchema, purgedKind); err != nil {
		return err
	}
	return tx.Commit()
}

// DirectoryTombstoned is the final purge check, not a write admission cache.
func DirectoryTombstoned(ctx context.Context, path string) (bool, error) {
	cat, err := root(ctx)
	if err != nil {
		return false, err
	}
	prefix, err := directoryPrefix(path)
	if err != nil {
		return false, err
	}
	var exists bool
	err = cat.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kypost_catalog.stores WHERE path=? AND kind=?)`, prefix, purgedKind).Scan(&exists)
	return exists, err
}
