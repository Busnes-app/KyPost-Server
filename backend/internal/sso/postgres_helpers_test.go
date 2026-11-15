package sso

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// rewriteAuthorityPostgresForTest is rewriteAuthorityForTest on Postgres: the same rows
// with edit applied, written with every trigger and foreign key lifted. A rewrite into
// another schema is a SQLite file test.
func rewriteAuthorityPostgresForTest(t *testing.T, s *LifecycleStore, schema string, edit func(*lifecycleFile, **nativeLedger)) {
	t.Helper()
	if schema != authoritySchemaV3 {
		t.Skip("SQLite only: rewrites the database under another schema")
	}
	ctx := context.Background()
	path := filepath.Join(filepath.Dir(s.path), NativeAuthorityFile)
	db, err := pgAuthorityPool(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	rows := authorityRows{}
	for _, table := range authorityTablesV3 {
		if err := readTable(ctx, db, table, rows); err != nil {
			t.Fatal(err)
		}
	}
	life, l, err := rows.decode()
	if err != nil {
		t.Fatal(err)
	}
	change := int64(0)
	if err := s.authority().View(ctx, func(r authorityReader) error {
		change, err = r.Change()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	edit(&life, &l)
	rows = authorityRows{}
	rows.add("meta", "change", strconv.FormatInt(change, 10))
	rows.add("meta", "format", "3")
	rows.add("meta", "initialized", strconv.FormatInt(flag(life.NativeProvisioningInitialized), 10))
	if err := rows.lifecycleRows(life); err != nil {
		t.Fatal(err)
	}
	if l != nil {
		rows.ledgerRows(*l)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	for _, table := range authorityTablesV3 {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows[table] {
			q := "INSERT INTO " + table + " VALUES(?" + strings.Repeat(",?", len(row)-1) + ")"
			if _, err := tx.Exec(q, pgRow(true, table, row)...); err != nil {
				t.Fatal(table, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// removeAuthorityForTest deletes the authority database on either backend.
func removeAuthorityForTest(t *testing.T, path string) {
	t.Helper()
	if onPostgres(path) {
		dropPGAuthorityPool(path)
		if err := dbbackend.RemoveFileForTest(path); err != nil {
			t.Fatal(err)
		}
	}
}
