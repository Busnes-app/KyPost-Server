package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Root is a data directory and its name in a capsule and the catalog ("state", "config").
type Root struct{ Dir, Prefix string }

// Counts are rows per table per catalog path.
type Counts map[string]map[string]int64

// importStep and recountStep let tests fail an import before its commit, or change what
// the recount saw.
var (
	importStep  = func() error { return nil }
	recountStep = func(c Counts) Counts { return c }
)

// ImportDatabases copies every store database under roots (the files a backup would
// collect: state.db, mailbox.db, ingress.db, cloudflare.db, native-authority.db) into the
// empty Postgres database in one transaction, then recounts every table after the commit
// and fails on any difference. It refuses a -wal beside any file and a database holding
// any row, before writing; it never writes a source (an older file is brought up to date
// on a private copy under scratch). It is migrate-to-postgres and restore into Postgres.
func ImportDatabases(ctx context.Context, roots []Root, scratch string) (Counts, error) {
	type source struct{ path, rel, kind string }
	var sources []source
	for _, r := range roots {
		err := filepath.WalkDir(r.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != r.Dir && skip(d.Name()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			k, ok := dbbackend.KindOf(d.Name())
			if d.IsDir() || !ok {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s is not a regular file", p)
			}
			if wal, err := os.Lstat(p + "-wal"); err == nil && wal.Size() > 0 {
				return fmt.Errorf("%s has a write-ahead log beside it: stop every KyPost process (a clean stop folds the log into the file) and try again", p)
			}
			rel, err := filepath.Rel(r.Dir, p)
			if err != nil {
				return err
			}
			sources = append(sources, source{p, r.Prefix + "/" + filepath.ToSlash(rel), k.Name})
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	before, err := dbbackend.RowCounts(ctx)
	if err != nil {
		return nil, err
	}
	if held := heldRows(before); len(held) > 0 {
		return nil, fmt.Errorf("%w (rows in %s); refusing to copy over it", dbbackend.ErrNotEmpty, strings.Join(held, ", "))
	}
	// Every source is opened (and refused if newer, or upgraded on a copy) before the
	// transaction starts.
	opened := make([]*sql.DB, len(sources))
	for i, s := range sources {
		db, done, err := dbbackend.OpenSQLiteSource(ctx, s.path, s.kind, scratch)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.rel, err)
		}
		defer done()
		opened[i] = db
	}
	tx, err := dbbackend.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Reading a large SQLite table between inserts must not trip the pool's 30 s idle limit.
	if _, err := tx.ExecContext(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return nil, err
	}
	copied := Counts{}
	for i, s := range sources {
		n, err := dbbackend.ImportSQLite(ctx, tx, s.rel, s.kind, opened[i])
		if err != nil {
			return nil, hintLocks(err)
		}
		copied[s.rel] = n
	}
	if err := importStep(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, hintLocks(err)
	}
	after, err := dbbackend.RowCounts(ctx)
	if err != nil {
		return copied, fmt.Errorf("recounting after the commit: %w", err)
	}
	after = recountStep(after)
	if rel, table, ok := countsDiffer(copied, after); ok {
		err := fmt.Errorf("after the commit %s table %s holds %d rows, but %d were copied", rel, table, after[rel][table], copied[rel][table])
		if undoErr := dbbackend.Undo(ctx, slices.Collect(maps.Keys(copied)), before); undoErr != nil {
			return copied, fmt.Errorf("%w; removing the copy failed too, so drop the database before trying again: %w", err, undoErr)
		}
		return copied, fmt.Errorf("%w; the copy was removed and the database is as it was", err)
	}
	return copied, nil
}

// countsDiffer finds a table whose recount differs from what was copied.
func countsDiffer(copied, after Counts) (rel, table string, differ bool) {
	for _, c := range []Counts{after, copied} {
		for _, rel := range slices.Sorted(maps.Keys(c)) {
			for _, table := range slices.Sorted(maps.Keys(c[rel])) {
				if copied[rel][table] != after[rel][table] {
					return rel, table, true
				}
			}
		}
	}
	return "", "", false
}

func heldRows(c Counts) []string {
	var held []string
	for _, rel := range slices.Sorted(maps.Keys(c)) {
		for table, n := range c[rel] {
			if n > 0 {
				held = append(held, rel+" "+table)
			}
		}
	}
	slices.Sort(held)
	return held
}

// hintLocks names the setting a large install's one-transaction copy needs: it locks
// every table of every account at once.
func hintLocks(err error) error {
	if strings.Contains(err.Error(), "max_locks_per_transaction") {
		return fmt.Errorf("%w (the copy is one transaction over every account's tables: raise max_locks_per_transaction, see docs/RUNNING.md)", err)
	}
	return err
}

// collectPostgres adds every catalogued database under the config and state roots as the
// SQLite member a SQLite install's backup holds, at the same path: each schema exported
// from its own snapshot, mail databases through the bulk stage when one is configured.
func (s *Service) collectPostgres(ctx context.Context, bulk *bulkStage, scratch, ingressPath string, add func(string, []byte) error) error {
	stores, err := dbbackend.Stores(ctx)
	if err != nil {
		return err
	}
	for _, st := range stores {
		live := st.LivePath()
		if st.Pending() || live == ingressPath {
			continue
		}
		name, ok := s.memberName(live)
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if bulk != nil && strings.HasPrefix(name, "state/") && bulkDatabase(filepath.Base(live)) {
			if err := bulk.snapshot(ctx, live, name); err != nil {
				return fmt.Errorf("collect %s: %w", name, err)
			}
			continue
		}
		raw, err := state.SnapshotDB(ctx, live, scratch)
		if err != nil {
			return fmt.Errorf("collect %s: %w", name, err)
		}
		if err := add(name, raw); err != nil {
			return err
		}
	}
	return nil
}

// memberName is a live database's capsule path, if it lies under the config or state root
// outside every directory a backup skips.
func (s *Service) memberName(live string) (string, bool) {
	for _, r := range []struct{ path, prefix string }{{s.dirs.Config, "config"}, {s.dirs.State, "state"}} {
		rel, err := filepath.Rel(r.path, live)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if skip(part) {
				return "", false
			}
		}
		if s.cfg.Dir != "" && within(s.cfg.Dir, live) {
			return "", false
		}
		return r.prefix + "/" + filepath.ToSlash(rel), true
	}
	return "", false
}

// RemoveDatabaseFiles deletes the store database files under roots (and any -wal, -shm or
// -journal beside them): after ImportDatabases, restore publishes no SQLite copy.
func RemoveDatabaseFiles(roots []Root) error {
	for _, r := range roots {
		err := filepath.WalkDir(r.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if _, ok := dbbackend.KindOf(d.Name()); ok {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					if err := os.Remove(p + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// SetImportHooksForTest fails ImportDatabases before its commit (beforeCommit) or changes
// what its recount sees (recount), until the test ends.
func SetImportHooksForTest(t testing.TB, beforeCommit func() error, recount func(Counts) Counts) {
	t.Helper()
	if !testing.Testing() {
		panic("backup: test hook called outside a test binary")
	}
	oldStep, oldRecount := importStep, recountStep
	if beforeCommit != nil {
		importStep = beforeCommit
	}
	if recount != nil {
		recountStep = recount
	}
	t.Cleanup(func() { importStep, recountStep = oldStep, oldRecount })
}
