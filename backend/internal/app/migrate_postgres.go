package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// migrateUsage: the command copies a stopped SQLite install into the empty Postgres
// database the variable names, in one transaction, and never changes the SQLite files.
const migrateUsage = "usage: KYPOST_DATABASE_URL_FILE=<file> kypost-server migrate-to-postgres [--check] -from <state dir> [-config <config dir>] (-from is required; -config defaults to CONFIG_DIR; the database URL comes only from KYPOST_DATABASE_URL or KYPOST_DATABASE_URL_FILE)"

// runMigrateToPostgres moves a stopped install's databases into Postgres: every
// database under the state dir and the config dir, one transaction, each table's rows
// printed and verified by count and content before the commit, recounted after it. It
// exits non-zero on any failure; a failure before the commit leaves Postgres as it was.
// --check does all of it and rolls back. It holds the SQLite server-instance lock, so it
// refuses while KyPost runs on that data.
func runMigrateToPostgres(args []string, stdout io.Writer) error {
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "postgres://") || strings.Contains(strings.ToLower(a), "postgresql://") {
			// Never echoed: it may carry a password.
			return errors.New("a database URL on the command line is refused, because argv is visible to every process and may be logged; set KYPOST_DATABASE_URL_FILE instead. " + migrateUsage)
		}
	}
	fs := flag.NewFlagSet("migrate-to-postgres", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the stopped install's STATE_DIR")
	configDir := fs.String("config", config.ConfigDir(), "the stopped install's CONFIG_DIR")
	check := fs.Bool("check", false, "copy and verify, then roll back")
	if err := fs.Parse(args); err != nil || *from == "" || fs.NArg() != 0 {
		return errors.New(migrateUsage)
	}
	if !dbbackend.Postgres() {
		return errors.New("set KYPOST_DATABASE_URL or KYPOST_DATABASE_URL_FILE to the target database; " + migrateUsage)
	}
	if _, err := os.Stat(*from); err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	release, err := state.HoldInstance(*from, true)
	if err != nil {
		return err
	}
	defer release()
	ctx := context.Background()
	if err := dbbackend.Check(ctx); err != nil {
		return err
	}
	if err := backup.RefusePinWithoutKey(*from); err != nil {
		return err
	}
	scratch, err := migrationScratch(*from)
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	roots := []backup.Root{{Dir: *configDir, Prefix: "config"}, {Dir: *from, Prefix: "state"}}
	importer := backup.ImportDatabases
	if *check {
		importer = backup.CheckImport
	}
	counts, err := importer(ctx, roots, scratch)
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
	if *check {
		fmt.Fprintf(stdout, "check passed: %d rows from %d databases copy to Postgres at %s with every count and content digest matching; nothing was kept.\n", rows, len(counts), dbbackend.Describe())
		return nil
	}
	fmt.Fprintf(stdout, "copied %d rows from %d databases to Postgres at %s; every count and content digest matched before the commit and every count after it.\n", rows, len(counts), dbbackend.Describe())
	fmt.Fprintln(stdout, "Start KyPost with the same database variable; it no longer reads the SQLite files. Once it checks out, move the .db files out of the volumes. There is no command back (restore a capsule with the variable unset).")
	return nil
}

// migrationScratch is a fresh 0700 directory for upgraded source copies under the state
// dir's backup scratch, never the system temp directory, after sweeping leftovers of
// earlier runs: real directories this user owns, named migrate-*. The exclusive instance
// lock is held, so nothing else is using them.
func migrationScratch(stateDir string) (string, error) {
	root := filepath.Join(stateDir, dbbackend.ScratchDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "migrate-") {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, e.Name()))
		if err != nil || !info.IsDir() {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Geteuid() {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	return os.MkdirTemp(root, "migrate-")
}
