package vol_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

func TestDocumentReferencesAndGuardedRemoval(t *testing.T) {
	table, _, _ := twoTables(t, "state")
	root := t.TempDir()
	release, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	contactPath := filepath.Join(root, "users", "u1", "contacts.json")
	photoPath := filepath.Join(root, "users", "u1", "contact-photos", "one.jpg")
	photo := vol.OpenDocument(photoPath)
	if err := photo.Replace([]byte("photo bytes")); err != nil {
		t.Fatal(err)
	}
	contact := vol.OpenDocument(contactPath)
	if _, err := contact.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// Shared photos are counted once, even when several contacts name them.
	if err := contact.WriteWithReferences([]byte("references one"), []string{photoPath, photoPath}); err != nil {
		t.Fatal(err)
	}
	oldGuard := vol.OpenDocument(contactPath)
	if _, err := oldGuard.Read(); err != nil {
		t.Fatal(err)
	}
	if err := contact.Replace([]byte("new reference set")); err != nil {
		t.Fatal(err)
	}
	if _, err := photo.Read(); err != nil {
		t.Fatal(err)
	}
	if err := photo.RemoveIfUnchanged(oldGuard); !errors.Is(err, vol.ErrStale) {
		t.Fatal("old reference snapshot deleted photo", err)
	}
	// ABA: a delayed remover cannot match another incarnation of the same path.
	stalePhoto := vol.OpenDocument(photoPath)
	if _, err := stalePhoto.Read(); err != nil {
		t.Fatal(err)
	}
	if err := photo.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := vol.OpenDocument(photoPath).Replace([]byte("new incarnation")); err != nil {
		t.Fatal(err)
	}
	if err := stalePhoto.RemoveIfUnchanged(contact); !errors.Is(err, vol.ErrStale) {
		t.Fatal("stale photo incarnation deleted replacement", err)
	}
	foreignPath := filepath.Join(root, "users", "u-other", "contact-photos", "foreign.jpg")
	if err := vol.OpenDocument(foreignPath).Replace([]byte("foreign photo")); err != nil {
		t.Fatal(err)
	}
	if err := contact.WriteWithReferences([]byte("foreign reference"), []string{foreignPath}); err == nil {
		t.Fatal("foreign owner reference accepted")
	}
	foreignGuard := vol.OpenDocument(filepath.Join(root, "users", "u-other", "contacts.json"))
	if err := photo.RemoveIfUnchanged(foreignGuard); err == nil {
		t.Fatal("foreign guard admitted deletion")
	}
	if _, err := photo.Read(); err != nil {
		t.Fatal(err)
	}
	if err := photo.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := contact.WriteWithReferences([]byte("missing reference"), []string{photoPath}); !errors.Is(err, vol.ErrStale) {
		t.Fatal("missing reference accepted", err)
	}
	body, err := vol.OpenDocument(contactPath).Read()
	if err != nil || string(body) != "new reference set" {
		t.Fatal("failed reference changed body", string(body), err)
	}
}

func TestMissingReferenceCreatesNoBodyOrOrgKey(t *testing.T) {
	options := tableOptions()
	options.Org = func(_ context.Context, user string) (string, error) { return "org-" + user, nil }
	table, _, db := twoTablesWithOptions(t, "state", options)
	root := t.TempDir()
	release, err := vol.Mount(root, table)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := filepath.Join(root, "users", "u1", "contacts.json")
	photo := filepath.Join(root, "users", "u1", "contact-photos", "missing.jpg")
	if err := vol.OpenDocument(path).WriteWithReferences([]byte("new contact"), []string{photo}); !errors.Is(err, vol.ErrStale) {
		t.Fatal("missing reference admitted", err)
	}
	if _, err := vol.OpenDocument(path).Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed reference left body", err)
	}
	var keys int
	if err := db.QueryRow(`SELECT count(*) FROM doc_keys WHERE org_id='org-u1'`).Scan(&keys); err != nil || keys != 0 {
		t.Fatal("failed reference created key", keys, err)
	}
}
