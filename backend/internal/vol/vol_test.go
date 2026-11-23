package vol_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// twoTables opens one volumes.db twice: two replicas sharing the documents table.
func twoTables(t *testing.T, root string) (*vol.Table, *vol.Table, *sql.DB) {
	return twoTablesWithOptions(t, root, tableOptions())
}

func twoTablesWithOptions(t *testing.T, root string, options vol.TableOptions) (*vol.Table, *vol.Table, *sql.DB) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	dir := t.TempDir()
	var tables [2]*vol.Table
	var first *sql.DB
	for i := range tables {
		db, err := volumes.Open(dir, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if tables[i], err = vol.NewTable(db, root, options); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = db
		}
	}
	return tables[0], tables[1], first
}

func tableOptions() vol.TableOptions {
	return vol.TableOptions{MasterKey: make([]byte, 32), Org: func(context.Context, string) (string, error) { return "", nil }}
}

var ctx = context.Background()

func TestStaleRevisionIsRefused(t *testing.T) {
	a, _, _ := twoTables(t, "config")
	const p = "users/u1/contacts.json"
	r1, err := a.Write(ctx, p, []byte("one"), 0)
	if err != nil || r1 <= 0 {
		t.Fatal(r1, err)
	}
	if _, err := a.Write(ctx, p, []byte("again"), 0); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("a create over an existing document: %v", err)
	}
	r2, err := a.Write(ctx, p, []byte("two"), r1)
	if err != nil || r2 <= r1 {
		t.Fatal(r2, err)
	}
	if _, err := a.Write(ctx, p, []byte("lost"), r1); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("an update at a stale revision: %v", err)
	}
	if err := a.Remove(ctx, p, r1); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("a remove at a stale revision: %v", err)
	}
	if _, err := a.Write(ctx, "users/u1/other.json", []byte("x"), r2); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("an update of an absent document: %v", err)
	}
	if body, rev, err := a.Read(ctx, p); string(body) != "two" || rev != r2 || err != nil {
		t.Fatalf("read %q %d %v", body, rev, err)
	}
	if err := a.Remove(ctx, p, r2); err != nil {
		t.Fatal(err)
	}
	if _, rev, err := a.Read(ctx, p); rev != 0 || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after remove: %d %v", rev, err)
	}
}

// Two replicas read one revision and both write: one wins, the other is stale, and its
// retry (vol.Update) sees the winner's body, so no update is lost.
func TestUpdateRaceOneWinsTheOtherRetries(t *testing.T) {
	a, b, _ := twoTables(t, "state")
	const p = "users/u1/mailcache.json"
	if _, err := a.Write(ctx, p, []byte("0"), 0); err != nil {
		t.Fatal(err)
	}
	_, ra, _ := a.Read(ctx, p)
	_, rb, _ := b.Read(ctx, p)
	if _, err := a.Write(ctx, p, []byte("a"), ra); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(ctx, p, []byte("b"), rb); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("the losing writer: %v", err)
	}
	applied := make([]bool, 4)
	var wg sync.WaitGroup
	for i, v := range []vol.Vol{a, b, a, b} {
		wg.Go(func() {
			err := updateDocument(ctx, v, p, func(body []byte) ([]byte, error) { return append(body, byte('0'+i)), nil })
			if err != nil && !errors.Is(err, vol.ErrBusy) {
				t.Error(err)
			}
			applied[i] = err == nil
		})
	}
	wg.Wait()
	body, _, _ := a.Read(ctx, p)
	if body[0] != 'a' {
		t.Fatalf("body %q", body)
	}
	// An vol.Update that returned nil applied exactly once; one that gave up applied nothing.
	for i, ok := range applied {
		if n := bytes.Count(body, []byte{byte('0' + i)}); n != map[bool]int{true: 1}[ok] {
			t.Fatalf("update %d (ok %v) is in %q %d times", i, ok, body, n)
		}
	}
}

// Two replicas create one document at once: exactly one wins, and the other gets vol.ErrStale,
// never a key violation.
func TestFirstCreateRace(t *testing.T) {
	a, b, _ := twoTables(t, "config")
	for i := range 20 {
		p := fmt.Sprintf("users/u%d/rules.json", i)
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, v := range []vol.Vol{a, b} {
			wg.Go(func() {
				<-start
				_, errs[j] = v.Write(ctx, p, []byte{byte('a' + j)}, 0)
			})
		}
		close(start)
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, vol.ErrStale):
				t.Fatalf("a racing create: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("%d creators won", won)
		}
	}
}

// A writer that read before a remove and re-create is refused however long it stalled:
// revisions never repeat, even with the same body.
func TestABAAfterRemoveAndRecreate(t *testing.T) {
	a, b, _ := twoTables(t, "config")
	const p = "users/u1/groups.json"
	stale, err := a.Write(ctx, p, []byte("same"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(ctx, p, stale); err != nil {
		t.Fatal(err)
	}
	fresh, err := b.Write(ctx, p, []byte("same"), 0)
	if err != nil || fresh == stale {
		t.Fatalf("re-created at %d (was %d): %v", fresh, stale, err)
	}
	if _, err := a.Write(ctx, p, []byte("paused"), stale); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("the paused writer: %v", err)
	}
	if err := a.Remove(ctx, p, stale); !errors.Is(err, vol.ErrStale) {
		t.Fatalf("the paused remover: %v", err)
	}
	if body, _, _ := a.Read(ctx, p); string(body) != "same" {
		t.Fatalf("body %q", body)
	}
}

// A writer beaten on every try gives up with vol.ErrBusy (503) and writes nothing of its own.
func TestRetryBoundGivesBusy(t *testing.T) {
	a, b, _ := twoTables(t, "config")
	const p = "config.yaml"
	if _, err := a.Write(ctx, p, []byte("v0"), 0); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := updateDocument(ctx, a, p, func(body []byte) ([]byte, error) {
		calls++
		_, rev, _ := b.Read(ctx, p)
		if _, err := b.Write(ctx, p, fmt.Appendf(nil, "b%d", calls), rev); err != nil {
			t.Fatal(err)
		}
		return []byte("mine"), nil
	})
	if !errors.Is(err, vol.ErrBusy) || calls != 5 {
		t.Fatalf("vol.Update: %v after %d calls", err, calls)
	}
	if body, _, _ := a.Read(ctx, p); string(body) != fmt.Sprintf("b%d", 5) {
		t.Fatalf("body %q", body)
	}
}

var badPaths = []string{
	"", "/etc/passwd", "/users/u1/contacts.json", "..", "../config.yaml", "users/../config.yaml",
	"users/u1/..", "./config.yaml", "users/./u1", "users//u1", "users/u1/", "users/u1\x00/x",
	`users\u1`, "users/\xff", strings.Repeat("a", vol.MaxPath+1),
}

// Every refusal happens before SQL: the handle's database is closed, so reaching it would
// fail with another error.
func TestBoundaryRefusesBeforeSQL(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	vols := map[string]vol.Vol{"file": vol.File{Dir: t.TempDir()}, "table": &vol.Table{}}
	for name, v := range vols {
		for _, p := range badPaths {
			if _, _, err := v.Read(ctx, p); !errors.Is(err, vol.ErrPath) {
				t.Errorf("%s read %q: %v", name, p, err)
			}
			if _, err := v.Write(ctx, p, nil, 0); !errors.Is(err, vol.ErrPath) {
				t.Errorf("%s write %q: %v", name, p, err)
			}
			if err := v.Remove(ctx, p, 1); !errors.Is(err, vol.ErrPath) {
				t.Errorf("%s remove %q: %v", name, p, err)
			}
			if _, err := v.List(ctx, p); !errors.Is(err, vol.ErrPath) {
				t.Errorf("%s list %q: %v", name, p, err)
			}
		}
		if _, err := v.Write(ctx, "config.yaml", make([]byte, vol.MaxBody+1), 0); name == "table" && !errors.Is(err, vol.ErrTooLarge) {
			t.Errorf("%s oversize body: %v", name, err)
		}
		if _, err := v.Write(ctx, "config.yaml", nil, -1); err == nil {
			t.Errorf("%s negative revision: %v", name, err)
		}
	}
	if err := vol.CheckPath(strings.Repeat("a", vol.MaxPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := vol.NewTable(nil, "config", tableOptions()); err == nil {
		t.Fatal(err)
	}
}

// The table's CHECK refuses what the validator does, for a writer that skips the seam.
func TestDocumentsCheckRefusesBadPaths(t *testing.T) {
	_, _, db := twoTables(t, "config")
	for _, p := range []string{"", "/x", "a/../b", "a//b", "a/", "./a", strings.Repeat("a", vol.MaxPath+1)} {
		if _, err := db.ExecContext(ctx, `INSERT INTO documents (root, path, body, updated_at) VALUES ('config', ?, ?, 0)`, p, []byte{}); err == nil {
			t.Errorf("the table accepted %q", p)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO documents (root, path, body, updated_at) VALUES ('secret', 'a', ?, 0)`, []byte{}); err == nil {
		t.Error("the table accepted another root")
	}
}

// The seam sets the owner from the path; mailbox and pickup documents wait for MH2.
func TestOwnerComesFromThePath(t *testing.T) {
	a, _, db := twoTables(t, "state")
	if _, err := a.Write(ctx, "users/u-9/carddav-auth.json", []byte("{}"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, "native-domains.json", []byte("{}"), 0); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]sql.NullString{"users/u-9/carddav-auth.json": {String: "u-9", Valid: true}, "native-domains.json": {}} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT owner FROM documents WHERE path = ?`, p).Scan(&got); err != nil || got != want {
			t.Errorf("%s owner %v %v", p, got, err)
		}
	}
	for _, p := range []string{"mailboxes/mbx-1/mailcache.json", "pickup/abc.json", "users"} {
		if _, err := a.Write(ctx, p, []byte("{}"), 0); err == nil {
			t.Errorf("%s written without an owner", p)
		}
	}
}

// List stays under its prefix, so neither a longer ID nor a LIKE wildcard widens it.
func TestListStaysUnderItsPrefix(t *testing.T) {
	a, b, _ := twoTables(t, "config")
	for _, p := range []string{"users/a/x.json", "users/a/sub/y.json", "users/ab/z.json", "users/a_/w.json", "users/a%/v.json", "users.json"} {
		if _, err := a.Write(ctx, p, []byte("1"), 0); err != nil {
			t.Fatal(err)
		}
	}
	f := vol.File{Dir: t.TempDir()}
	for _, p := range []string{"users/a/x.json", "users/a/sub/y.json", "users/ab/z.json"} {
		if _, err := f.Write(ctx, p, []byte("1"), 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []vol.Vol{b, f} {
		got, err := v.List(ctx, "users/a")
		if err != nil || strings.Join(got, ",") != "users/a/sub/y.json,users/a/x.json" {
			t.Errorf("%T list: %v %v", v, got, err)
		}
	}
	if got, err := f.List(ctx, "absent"); err != nil || len(got) != 0 {
		t.Errorf("absent prefix: %v %v", got, err)
	}
}

// vol.File is today's fsutil write: the same bytes at the same path, mode 0600.
func TestFileWritesWhatTheStoresWrite(t *testing.T) {
	dir := t.TempDir()
	f := vol.File{Dir: dir}
	if _, _, err := f.Read(ctx, "users/u1/contacts.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := f.Write(ctx, "users/u1/contacts.json", []byte(`{"a":1}`), 0); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "users", "u1", "contacts.json")
	b, err := os.ReadFile(full)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatal(string(b), err)
	}
	if fi, _ := os.Stat(full); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if body, rev, err := f.Read(ctx, "users/u1/contacts.json"); string(body) != `{"a":1}` || rev != 1 || err != nil {
		t.Fatal(string(body), rev, err)
	}
	if err := f.Remove(ctx, "users/u1/contacts.json", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(full); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// Update is a read-modify-write: change gets the current body (nil when absent) and returns
// the new one; a stale write is retried, at most 5 times, then vol.ErrBusy. Nothing is
// written partially.
func updateDocument(ctx context.Context, v vol.Vol, path string, change func(body []byte) ([]byte, error)) error {
	for range 5 {
		body, rev, err := v.Read(ctx, path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		next, err := change(body)
		if err != nil {
			return err
		}
		if _, err := v.Write(ctx, path, next, rev); !errors.Is(err, vol.ErrStale) {
			return err
		}
	}
	return vol.ErrBusy
}
