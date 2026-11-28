package api

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
)

// The scheduled pass reaches every native mailbox's SweepBodies (MH11): an unnamed object is
// first recorded, then deleted once a sighting is older than the retention.
func TestBodySweepVisitsEveryMailbox(t *testing.T) {
	px := newPurgeFixture(t)
	cell := objecttest.Cell()
	objects, _ := objecttest.New(t, cell)
	px.srv.UseObjects(objects)
	mailbox.SetObjectBodies(func(mailbox.Owner) (*vol.Object, mailbox.BodyScope, error) {
		return objects, mailbox.BodyScope{Cell: cell, Org: orgA}, nil
	})
	defer mailbox.SetObjectBodies(nil)
	ctx := context.Background()
	orphans := map[string]string{}
	for _, mb := range []string{px.a.ID, px.a.extra} {
		id, err := vol.NewObjectID()
		if err != nil {
			t.Fatal(err)
		}
		orphans[mb] = vol.BodyPrefix(cell, orgA, mb) + id
		if _, err := objects.Write(ctx, orphans[mb], []byte("never named"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := px.srv.sweepBodies(ctx); n != 0 {
		t.Fatalf("first pass deleted %d, want 0 (sightings only)", n)
	}
	for mb := range orphans {
		a, ok, err := px.srv.ssoLifecycle.NativeMailboxAssignment(mb)
		if err != nil || !ok {
			t.Fatal(mb, ok, err)
		}
		path := filepath.Join(a.Dir(px.srv.stateDir), "mailbox", "mailbox.db")
		var db *sql.DB
		if dbbackend.Postgres() {
			db, err = dbbackend.Open(path, dbbackend.Options{Kind: mailbox.Kind})
		} else {
			db, err = sql.Open("sqlite", path)
		}
		if err != nil {
			t.Fatal(err)
		}
		res, err := db.Exec("UPDATE body_orphans SET seen_at=seen_at-?", int64((mailbox.BodyRetention + time.Hour).Seconds()))
		_ = db.Close()
		if n, _ := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("mailbox %s: %d sightings, %v", mb, n, err)
		}
	}
	if n := px.srv.sweepBodies(ctx); n != len(orphans) {
		t.Fatalf("second pass deleted %d, want %d", n, len(orphans))
	}
	for mb, key := range orphans {
		if keys, err := objects.List(ctx, vol.BodyPrefix(cell, orgA, mb)); err != nil || len(keys) != 0 {
			t.Fatalf("mailbox %s keeps %v (%s): %v", mb, keys, key, err)
		}
	}
}
