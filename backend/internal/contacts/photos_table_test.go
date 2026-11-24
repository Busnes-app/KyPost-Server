package contacts

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func tablePhotoStores(t *testing.T) (*Store, *Store, string) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	db, err := volumes.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO accounts(id,username,record) VALUES('photo-owner','photo-owner',?)`, []byte(`{"id":"photo-owner"}`)); err != nil {
		t.Fatal(err)
	}
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: bytes.Repeat([]byte{3}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	release, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	dir := filepath.Join(root, "users", "photo-owner")
	a, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Upsert(Contact{UID: "target", FormattedName: "Target"}); err != nil {
		t.Fatal(err)
	}
	return a, b, filepath.Join(dir, "contact-photos")
}

func TestTablePhotoSweepRetainsConcurrentlyReferencedPhoto(t *testing.T) {
	a, b, dir := tablePhotoStores(t)
	for _, name := range []string{"old.jpg", "new.jpg"} {
		if err := vol.OpenDocument(filepath.Join(dir, name)).Replace([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := a.SetPhotoRef("target", "old.jpg"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var once sync.Once
	photoSweepSnapshot = func() {
		once.Do(func() {
			if _, ok, err := b.SetPhotoRef("target", "new.jpg"); err != nil || !ok {
				t.Fatal(ok, err)
			}
		})
	}
	t.Cleanup(func() { photoSweepSnapshot = nil })
	if err := a.SweepPhotos(t.Context()); err != nil {
		t.Fatal(err)
	}
	photo, err := vol.OpenDocument(filepath.Join(dir, "new.jpg")).Read()
	if err != nil || string(photo) != "new.jpg" {
		t.Fatal("sweep deleted a newly referenced photo", string(photo), err)
	}
	if _, err := vol.OpenDocument(filepath.Join(dir, "old.jpg")).Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unreferenced old photo was retained", err)
	}
}

func TestTableContactRefusesPhotoRemovedBeforeReferenceCommit(t *testing.T) {
	a, _, dir := tablePhotoStores(t)
	path := filepath.Join(dir, "pending.jpg")
	if err := vol.OpenDocument(path).Replace([]byte("pending bytes")); err != nil {
		t.Fatal(err)
	}
	if err := a.SweepPhotos(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := vol.OpenDocument(filepath.Join(a.baseDir, "contacts.json")).Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.SetPhotoRef("target", "pending.jpg"); err == nil {
		t.Fatal("published reference to a photo removed by the sweep")
	}
	after, err := vol.OpenDocument(filepath.Join(a.baseDir, "contacts.json")).Read()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed reference changed stored contact bytes", err)
	}
	current, ok, err := a.Get("target")
	if err != nil || !ok || current.PhotoRef != "" {
		t.Fatal("failed publication changed contact", current, ok, err)
	}
}

func TestTablePhotoSweepCancellationPreservesBytes(t *testing.T) {
	a, _, dir := tablePhotoStores(t)
	path := filepath.Join(dir, "orphan.jpg")
	if err := vol.OpenDocument(path).Replace([]byte("retained orphan")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	photoSweepSnapshot = cancel
	t.Cleanup(func() { photoSweepSnapshot = nil })
	if err := a.SweepPhotos(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled sweep continued", err)
	}
	data, err := vol.OpenDocument(path).Read()
	if err != nil || string(data) != "retained orphan" {
		t.Fatal("cancelled sweep removed bytes", string(data), err)
	}
}
