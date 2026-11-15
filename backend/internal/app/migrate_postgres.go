package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
)

// migrateUsage: the command copies a stopped SQLite install into the empty Postgres
// database the variable names, in one transaction, and never changes the SQLite files.
const migrateUsage = "usage: KYPOST_DATABASE_URL_FILE=<file> kypost-server migrate-to-postgres -from <state dir> [-config <config dir>] (-from is required; -config defaults to CONFIG_DIR)"

// runMigrateToPostgres moves a stopped install's databases into Postgres: every
// database under the state dir and the config dir, one transaction, each table's rows
// printed, recounted after the commit. It exits non-zero on any failure; a failure
// before the commit leaves Postgres as it was.
func runMigrateToPostgres(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate-to-postgres", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the stopped install's STATE_DIR")
	configDir := fs.String("config", config.ConfigDir(), "the stopped install's CONFIG_DIR")
	if err := fs.Parse(args); err != nil || *from == "" || fs.NArg() != 0 {
		return errors.New(migrateUsage)
	}
	if !dbbackend.Postgres() {
		return errors.New("set KYPOST_DATABASE_URL or KYPOST_DATABASE_URL_FILE to the target database; " + migrateUsage)
	}
	ctx := context.Background()
	if err := dbbackend.Check(ctx); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "kypost-migrate-") // 0700: older files are upgraded on copies here
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	counts, err := backup.ImportDatabases(ctx, []backup.Root{{Dir: *configDir, Prefix: "config"}, {Dir: *from, Prefix: "state"}}, scratch)
	var rows int64
	for _, rel := range slices.Sorted(maps.Keys(counts)) {
		for _, table := range slices.Sorted(maps.Keys(counts[rel])) {
			fmt.Fprintf(stdout, "%s\t%s\t%d\n", rel, table, counts[rel][table])
			rows += counts[rel][table]
		}
	}
	if err != nil {
		return fmt.Errorf("migrate-to-postgres failed: %w", err)
	}
	if len(counts) == 0 {
		return fmt.Errorf("migrate-to-postgres: no KyPost databases under %s or %s", *from, *configDir)
	}
	fmt.Fprintf(stdout, "copied %d rows from %d databases to Postgres at %s; every count matched after the commit.\n", rows, len(counts), dbbackend.Describe())
	fmt.Fprintln(stdout, "Start KyPost with the same database variable; it no longer reads the SQLite files. Once it checks out, move the .db files out of the volumes. There is no command back (restore a capsule with the variable unset).")
	return nil
}
