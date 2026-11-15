package mailbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// snapshotFile is a SQLite file holding the database at path: the file itself, or on
// Postgres an export of its schema, as a backup makes.
func snapshotFile(t *testing.T, path string) string {
	t.Helper()
	if !dbbackend.Postgres() {
		return path
	}
	dest := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := dbbackend.ExportSQLite(context.Background(), path, dest); err != nil {
		t.Fatal(err)
	}
	return dest
}

// skipFileCase skips a case that changes a database file or moves a directory: on
// Postgres the databases are schemas found by path, not files in the directory.
func skipFileCase(t *testing.T, name string) {
	t.Helper()
	for _, f := range []string{".db", "retargeted", "replace-", "corrupt-", "empty-mailbox"} {
		if strings.Contains(name, f) {
			dbbackend.SkipOnPostgres(t, "changes a database file in place")
		}
	}
}
