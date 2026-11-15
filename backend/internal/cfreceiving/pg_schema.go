package cfreceiving

import (
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// pgSchema is schema on Postgres, column names as on SQLite, BIGINT for INTEGER.
const pgSchema = `
CREATE TABLE IF NOT EXISTS tables (digest TEXT PRIMARY KEY, routes TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revisions (revision BIGINT PRIMARY KEY, issued_at BIGINT NOT NULL, digest TEXT NOT NULL REFERENCES tables(digest), installed_at BIGINT NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS ledger (key TEXT PRIMARY KEY, digest TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('junk','imported','quarantined','refused')), updated BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS blocks (digest TEXT PRIMARY KEY REFERENCES tables(digest), blocks TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS status (id BIGINT PRIMARY KEY CHECK(id=1), doc TEXT NOT NULL);`

// Kind is the catalog kind of a cloudflare.db.
const Kind = "cloudflare"

func init() {
	dbbackend.Register(dbbackend.Kind{Name: Kind, File: DBFile, DDL: pgSchema, CreateSQLite: func(path string) error {
		d, err := openDB(filepath.Dir(path), false)
		if err != nil {
			return err
		}
		return d.Close()
	}})
}
