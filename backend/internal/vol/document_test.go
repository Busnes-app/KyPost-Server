package vol_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

func TestDocumentPhotoDirectoryBoundAcrossReplicas(t *testing.T) {
	a, b, _ := twoTables(t, "state")
	roots := []string{filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "state")}
	for i, table := range []*vol.Table{a, b} {
		release, err := vol.Mount(roots[i], table)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	// Include all GCM base64 padding lengths. These must count plaintext bytes exactly.
	for i, n := range []int{1, 2, 3} {
		doc := vol.OpenDocument(filepath.Join(roots[0], "users/u/contact-photos", string(rune('a'+i))))
		if err := doc.WriteWithDirectoryLimit(bytes.Repeat([]byte{1}, n), 6); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := vol.DirectoryBytes(filepath.Join(roots[1], "users/u/contact-photos")); err != nil || n != 6 {
		t.Fatal(n, err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			<-start
			doc := vol.OpenDocument(filepath.Join(roots[i], "users/u/contact-photos", string(rune('x'+i))))
			results <- doc.WriteWithDirectoryLimit([]byte{1}, 7)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	successes, full := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, vol.ErrDirectoryFull) {
			full++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || full != 1 {
		t.Fatal(successes, full)
	}
	if n, err := vol.DirectoryBytes(filepath.Join(roots[0], "users/u/contact-photos")); err != nil || n != 7 {
		t.Fatal(n, err)
	}
}

func TestPlatformMarkerMoveIsAtomicAndRevisionChecked(t *testing.T) {
	a, b, db := twoTables(t, "state")
	root := filepath.Join(t.TempDir(), "state")
	release, err := vol.Mount(root, a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	const source = "native-restore-hold.json"
	rev, err := a.Write(ctx, source, []byte(`{"epoch":"old"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	doc := vol.OpenDocument(filepath.Join(root, source))
	if _, err := doc.Read(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(ctx, source, []byte(`{"epoch":"new"}`), rev); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "native-restore-released.json")
	if err := doc.MoveTo(dest); !errors.Is(err, vol.ErrStale) {
		t.Fatal("stale move", err)
	}
	if _, err := doc.Read(); err != nil {
		t.Fatal(err)
	}
	if err := doc.MoveTo(dest); err != nil {
		t.Fatal(err)
	}
	if exists, err := vol.Exists(filepath.Join(root, source)); err != nil || exists {
		t.Fatal("hold survived", exists, err)
	}
	if body, err := vol.OpenDocument(dest).Read(); err != nil || string(body) != `{"epoch":"new"}` {
		t.Fatal(string(body), err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM doc_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote a local marker", err)
	}
}

func TestDocumentCapLeavesThePreviousRevision(t *testing.T) {
	a, _, db := twoTables(t, "config")
	rev, err := a.Write(ctx, "config.yaml", []byte("old"), 0)
	if err != nil {
		t.Fatal(err)
	}
	large := make([]byte, vol.MaxBody+1)
	if _, err := a.Write(ctx, "config.yaml", large, rev); !errors.Is(err, vol.ErrTooLarge) {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM doc_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	body, next, err := a.Read(ctx, "config.yaml")
	if err != nil || next != rev || string(body) != "old" {
		t.Fatal(next, err)
	}
	// This is the owner's new table limit; the ordinary file path keeps accepting it.
	path := filepath.Join(t.TempDir(), "large.json")
	if err := vol.OpenDocument(path).Write(large); err != nil {
		t.Fatal("file behavior changed", err)
	}
}

// Admission must wait before reading usage, even with an existing org key.
// The store driver's shared writer transaction is the serialization boundary.
func TestDocumentDirectoryAdmissionWaitsForWriter(t *testing.T) {
	a, b, db := twoTables(t, "state")
	const prefix = "users/u/contact-photos/"
	if _, err := a.Write(ctx, prefix+"seed", []byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	release, err := vol.Mount(root, b)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	held, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback() }()
	done := make(chan error, 1)
	go func() {
		done <- vol.OpenDocument(filepath.Join(root, prefix+"next")).WriteWithDirectoryLimit([]byte{2}, 2)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("admission crossed the held writer: %v", err)
		default:
		}
		var waiting bool
		if err := held.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND pg_backend_pid() = ANY(pg_blocking_pids(pid)))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica never waited on the shared writer")
		}
		time.Sleep(time.Millisecond)
	}
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if used, err := vol.DirectoryBytes(filepath.Join(root, "users/u/contact-photos")); err != nil || used != 2 {
		t.Fatal(used, err)
	}
}
