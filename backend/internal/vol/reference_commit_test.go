package vol_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

func TestReferencePublicationAndCleanupShareWriterTransaction(t *testing.T) {
	table, _, db := twoTables(t, "state")
	root := t.TempDir()
	unmount, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	defer unmount()
	contactPath := filepath.Join(root, "users", "u1", "contacts.json")
	photoPath := filepath.Join(root, "users", "u1", "contact-photos", "pending.jpg")
	contact := vol.OpenDocument(contactPath)
	if err := contact.Replace([]byte("old reference set")); err != nil {
		t.Fatal(err)
	}
	guard := vol.OpenDocument(contactPath)
	if _, err := guard.Read(); err != nil {
		t.Fatal(err)
	}
	photo := vol.OpenDocument(photoPath)
	if err := photo.Replace([]byte("photo bytes")); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	writerKey, _ := pgdialect.LockKeys(schema + ",public")
	barrierKey := time.Now().UnixNano()
	// The UPDATE trigger is reached only after dependency validation and sealing,
	// and before the contact transaction commits. No production test hook is needed.
	if _, err := db.Exec(`CREATE FUNCTION hold_reference_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(` + fmt.Sprint(barrierKey) + `); RETURN NEW; END $$;
 CREATE TRIGGER hold_reference_commit BEFORE UPDATE ON documents FOR EACH ROW WHEN (OLD.path='users/u1/contacts.json') EXECUTE FUNCTION hold_reference_commit()`); err != nil {
		t.Fatal(err)
	}
	observer, err := sql.Open("pgx", os.Getenv(dbbackend.EnvTestURL))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	barrier, err := observer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	if _, err := barrier.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, barrierKey); err != nil {
		t.Fatal(err)
	}
	unlock := func() { _, _ = barrier.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, barrierKey) }
	defer unlock()
	publication := make(chan error, 1)
	go func() { publication <- contact.WriteWithReferences([]byte("new reference set"), []string{photoPath}) }()
	waitFor := func(key int64, finished <-chan error) {
		t.Helper()
		for {
			if finished != nil {
				select {
				case err := <-finished:
					t.Fatal("cleanup escaped publication's writer transaction", err)
				default:
				}
			}
			var waiting int
			err := observer.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=1`, int64(uint64(key)>>32), int64(uint64(key)&0xffffffff)).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting == 1 {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("expected writer wait was not observed", ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitFor(barrierKey, nil)
	cleanup := make(chan error, 1)
	go func() { cleanup <- photo.RemoveIfUnchanged(guard) }()
	waitFor(writerKey, cleanup)
	unlock()
	if err := <-publication; err != nil {
		t.Fatal("reference publication", err)
	}
	if err := <-cleanup; !errors.Is(err, vol.ErrStale) {
		t.Fatal("old cleanup snapshot admitted after publication", err)
	}
	if data, err := vol.OpenDocument(photoPath).Read(); err != nil || string(data) != "photo bytes" {
		t.Fatal("published photo was deleted", string(data), err)
	}
	if data, err := vol.OpenDocument(contactPath).Read(); err != nil || string(data) != "new reference set" {
		t.Fatal("publication did not commit", string(data), err)
	}
}
