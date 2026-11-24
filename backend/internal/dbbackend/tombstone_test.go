package dbbackend

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
)

var tombstoneTestOptions = Options{Kind: "state", DDL: `CREATE TABLE IF NOT EXISTS retained(id BIGINT PRIMARY KEY, body TEXT NOT NULL)`, Create: true}

func TestDirectoryTombstoneBlocksAllCreationPaths(t *testing.T) {
	FreshDatabaseForTest(t)
	ctx := context.Background()
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	path := filepath.Join(gone, "state.db")
	db, err := Open(path, tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sibling, err := Open(filepath.Join(root, "gone-sibling", "state.db"), tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	if _, err = sibling.Exec(`INSERT INTO retained VALUES(1,'sibling')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = TombstoneDir(ctx, gone); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{path, filepath.Join(gone, "never-published", "mailbox.db")} {
		for _, options := range []Options{tombstoneTestOptions, {Kind: "state", ReadOnly: true}, {Create: true}} {
			opened, err := Open(target, options)
			if err == nil {
				opened.Close()
				t.Fatal("purged open succeeded", options)
			}
			if !errors.Is(err, ErrPurged) || !errors.Is(err, ErrNotFound) {
				t.Fatal("wrong refusal", err)
			}
		}
	}
	if held, err := DirectoryTombstoned(ctx, gone); err != nil || !held {
		t.Fatal("marker missing", held, err)
	}
	if err = Remove(gone); err != nil {
		t.Fatal(err)
	}
	if held, err := DirectoryTombstoned(ctx, gone); err != nil || !held {
		t.Fatal("ordinary removal erased marker", held, err)
	}
	if exists, err := Exists(path); err != nil || exists {
		t.Fatal("purged store exists", exists, err)
	}
	var body string
	if err = sibling.QueryRow(`SELECT body FROM retained WHERE id=1`).Scan(&body); err != nil || body != "sibling" {
		t.Fatal("sibling prefix was purged", body, err)
	}
	stores, err := Stores(ctx)
	if err != nil || len(stores) != 1 {
		t.Fatal("markers listed as stores", stores, err)
	}
	counts, err := RowCounts(ctx)
	if err != nil || len(counts) != 1 {
		t.Fatal("markers counted as live schemas", counts, err)
	}
	tx, err := Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	schema, rel, err := SchemaName(filepath.Join(gone, "another.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = CreateSchemaTx(ctx, tx, schema, rel, "state", tombstoneTestOptions.DDL); !errors.Is(err, ErrPurged) {
		t.Fatal("direct schema create bypassed tombstone", err)
	}
}

func TestDirectoryTombstoneRefusesPendingPublication(t *testing.T) {
	FreshDatabaseForTest(t)
	ctx := context.Background()
	root := t.TempDir()
	from := filepath.Join(root, "staged", "state.db")
	gone := filepath.Join(root, "purged")
	target := filepath.Join(gone, "state.db")
	db, err := Open(from, tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO retained VALUES(1,'unpublished')`); err != nil {
		t.Fatal(err)
	}
	if err = TombstoneDir(ctx, gone); err != nil {
		t.Fatal(err)
	}
	callback := false
	if err = Stage(ctx, map[string]string{from: target}, func() error { callback = true; return nil }); !errors.Is(err, ErrPurged) || callback {
		t.Fatal("publication crossed tombstone", callback, err)
	}
	if err = Settle(ctx, target); !errors.Is(err, ErrPurged) {
		t.Fatal("settle accepted tombstone", err)
	}
	if err = Unstage(ctx, target); err != nil {
		t.Fatal(err)
	}
	if exists, err := Exists(from); err != nil || !exists {
		t.Fatal("refused publication changed source", exists, err)
	}
	var body string
	if err = db.QueryRow(`SELECT body FROM retained WHERE id=1`).Scan(&body); err != nil || body != "unpublished" {
		t.Fatal("refused publication changed bytes", body, err)
	}
	if held, err := DirectoryTombstoned(ctx, gone); err != nil || !held {
		t.Fatal("cleanup erased tombstone", held, err)
	}
}

func TestDirectoryTombstoneRollsBackDropsAndRetries(t *testing.T) {
	FreshDatabaseForTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.db"), tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO retained VALUES(1,'kept')`); err != nil {
		t.Fatal(err)
	}
	before, err := RowCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := root(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cat.Exec(`CREATE FUNCTION fail_purge_marker() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected marker failure'; END $$;
 CREATE TRIGGER fail_purge_marker BEFORE INSERT ON kypost_catalog.stores FOR EACH ROW WHEN (NEW.kind='purged') EXECUTE FUNCTION fail_purge_marker()`); err != nil {
		t.Fatal(err)
	}
	if err = TombstoneDir(ctx, dir); err == nil {
		t.Fatal("failed marker insert committed schema drops")
	}
	if after, err := RowCounts(ctx); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed purge changed schemas/rows", after, err)
	}
	if held, err := DirectoryTombstoned(ctx, dir); err != nil || held {
		t.Fatal("failed purge left marker", held, err)
	}
	var body string
	if err = db.QueryRow(`SELECT body FROM retained WHERE id=1`).Scan(&body); err != nil || body != "kept" {
		t.Fatal("failed purge lost data", body, err)
	}
	if _, err = cat.Exec(`DROP TRIGGER fail_purge_marker ON kypost_catalog.stores; DROP FUNCTION fail_purge_marker()`); err != nil {
		t.Fatal(err)
	}
	if err = TombstoneDir(ctx, dir); err != nil {
		t.Fatal("retry", err)
	}
	if held, err := DirectoryTombstoned(ctx, dir); err != nil || !held {
		t.Fatal("retry marker missing", held, err)
	}
}

func TestDirectoryTombstoneOrdersWritersAndCancellation(t *testing.T) {
	for _, level := range []sql.IsolationLevel{sql.LevelDefault, sql.LevelRepeatableRead, sql.LevelSerializable} {
		t.Run(level.String(), func(t *testing.T) { testTombstoneWriterOrder(t, level) })
	}
}

func testTombstoneWriterOrder(t *testing.T, isolation sql.IsolationLevel) {
	FreshDatabaseForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := Open(path, tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	probe, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	if err = probe.QueryRow(`SHOW transaction_isolation`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	want := map[sql.IsolationLevel]string{sql.LevelDefault: "read committed", sql.LevelRepeatableRead: "repeatable read", sql.LevelSerializable: "serializable"}[isolation]
	if actual != want {
		t.Fatalf("writer isolation downgraded: got %q want %q", actual, want)
	}
	if err = probe.Rollback(); err != nil {
		t.Fatal(err)
	}
	observer, err := sql.Open("pgx", current().url)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	schema, _, err := SchemaName(path)
	if err != nil {
		t.Fatal(err)
	}
	writerKey, _ := pgdialect.LockKeys(schema + ",public")
	waitFor := func(n int) {
		t.Helper()
		for {
			var waiting int
			err := observer.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=1`, int64(uint64(writerKey)>>32), int64(uint64(writerKey)&0xffffffff)).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting >= n {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("writer wait not observed", ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	first, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if _, err = first.Exec(`INSERT INTO retained VALUES(1,'before purge')`); err != nil {
		t.Fatal(err)
	}
	aborted, abort := context.WithCancel(ctx)
	cancelled := make(chan error, 1)
	go func() { cancelled <- TombstoneDir(aborted, dir) }()
	waitFor(1)
	abort()
	if err = <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled purge", err)
	}
	if err = first.Commit(); err != nil {
		t.Fatal("cancelled purge changed writer", err)
	}
	if held, err := DirectoryTombstoned(ctx, dir); err != nil || held {
		t.Fatal("cancelled purge left marker", held, err)
	}
	second, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	if _, err = second.Exec(`INSERT INTO retained VALUES(2,'ordered before purge')`); err != nil {
		t.Fatal(err)
	}
	purged := make(chan error, 1)
	go func() { purged <- TombstoneDir(ctx, dir) }()
	waitFor(1)
	stale := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: isolation})
		if err == nil {
			_, err = tx.ExecContext(ctx, `CREATE TABLE must_not_reappear(id BIGINT PRIMARY KEY)`)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		stale <- err
	}()
	waitFor(2)
	if err = second.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-purged; err != nil {
		t.Fatal("ordered purge", err)
	}
	if err = <-stale; !errors.Is(err, pgdialect.ErrSchemaGone) {
		t.Fatal("queued stale writer was not refused", err)
	}
	if held, err := DirectoryTombstoned(ctx, dir); err != nil || !held {
		t.Fatal("ordered purge marker missing", held, err)
	}
}

func TestOrdinaryRemovalStillAllowsRecreation(t *testing.T) {
	FreshDatabaseForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := Open(path, tombstoneTestOptions)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = Remove(dir); err != nil {
		t.Fatal(err)
	}
	if held, err := DirectoryTombstoned(context.Background(), dir); err != nil || held {
		t.Fatal("ordinary removal created marker", held, err)
	}
	db, err = Open(path, tombstoneTestOptions)
	if err != nil {
		t.Fatal("ordinary recreation refused", err)
	}
	db.Close()
}
