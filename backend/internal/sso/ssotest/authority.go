package ssotest

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

// Native authority (the directory lifecycle and the native ledger) is stored
// in native-authority.db beside two stubs that keep these names (v3, see
// docs/NATIVE_AUTHORITY_PER_ACCOUNT.md). Tests name the files only through
// these helpers, so a storage change edits one place; sso's
// TestAuthorityFixturePaths keeps them equal to the store's own paths.

// LifecyclePath is the lifecycle stub, sso-lifecycle.json (v1/v2: the record).
func LifecyclePath(configDir string) string {
	return filepath.Join(configDir, "sso-lifecycle.json")
}

// LedgerPath is the ledger stub, native-provisioning.json (v1/v2: the ledger).
func LedgerPath(configDir string) string {
	return filepath.Join(configDir, "native-provisioning.json")
}

// AuthorityPath is the v3 authority database.
func AuthorityPath(configDir string) string {
	return filepath.Join(configDir, "native-authority.db")
}

// DirectoryLockPath is the flock file of the directory fence.
func DirectoryLockPath(configDir string) string {
	return LifecyclePath(configDir) + ".lock"
}

// AuthorityContent is every authority row (read-only, sorted) plus both
// JSON files, so a test can tell whether authority changed. It does not
// depend on WAL checkpoints, which move bytes without changing rows.
func AuthorityContent(configDir string) ([]byte, error) {
	var out []string
	for _, path := range []string{LifecyclePath(configDir), LedgerPath(configDir)} {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		out = append(out, filepath.Base(path)+": "+string(raw))
	}
	path, err := filepath.Abs(AuthorityPath(configDir))
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return []byte(strings.Join(out, "\n")), nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := db.Query("SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := errors.Join(tables.Err(), tables.Close()); err != nil {
		return nil, err
	}
	for _, name := range names {
		rows, err := db.Query("SELECT * FROM " + name)
		if err != nil {
			return nil, err
		}
		columns, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			values := make([]any, len(columns))
			dst := make([]any, len(columns))
			for i := range values {
				dst[i] = &values[i]
			}
			if err := rows.Scan(dst...); err != nil {
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("%s %q", name, values))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		slices.Sort(lines)
		out = append(out, lines...)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// DamageAuthority runs one statement on the authority database with every
// trigger lifted for it (then restored with its exact text): a write from
// outside the store, such as a crash state no store write can produce.
func DamageAuthority(configDir, query string, args ...any) error {
	path, err := filepath.Abs(AuthorityPath(configDir))
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_txlock=immediate")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query("SELECT name, sql FROM sqlite_schema WHERE type = 'trigger'")
	if err != nil {
		return err
	}
	triggers := map[string]string{}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			return err
		}
		triggers[name] = text
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for name := range triggers {
		if _, err := tx.Exec("DROP TRIGGER " + name); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}
	for _, text := range triggers {
		if _, err := tx.Exec(text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LifecycleRecord is the JSON of one lifecycle singleton (recoveryRepair,
// recoveryChallenge, recoveryReceipt, restoreRelease), nil when absent.
func LifecycleRecord(configDir, name string) ([]byte, error) {
	path, err := filepath.Abs(AuthorityPath(configDir))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var raw []byte
	err = db.QueryRow("SELECT json FROM lifecycle_records WHERE name = ?", name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}
