package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgdialect"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

func TestTablePurgeStorageRefusesRecreatedSchema(t *testing.T) {
	dbbackend.FreshDatabaseForTest(t)
	fx, _ := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	dir := fx.srv.userStateDir(fx.a.ID)
	if err := PurgeStorage(context.Background(), fx.srv.configDir, fx.srv.stateDir, []string{fx.a.ID}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := dbbackend.Open(filepath.Join(dir, "state.db"), dbbackend.Options{Kind: "state", Create: true})
	if err == nil {
		db.Close()
		t.Fatal("purged directory's schema was recreated")
	}
	if !errors.Is(err, dbbackend.ErrPurged) {
		t.Fatal("wrong purge refusal", err)
	}
}

func TestTablePurgeStorageRefusesStaleSchemaWriter(t *testing.T) {
	dbbackend.FreshDatabaseForTest(t)
	fx, _ := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	dir := fx.srv.userStateDir(fx.a.ID)
	stale, err := dbbackend.Open(filepath.Join(dir, "state.db"), dbbackend.Options{Kind: "state"})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if err = stale.Ping(); err != nil {
		t.Fatal(err)
	}
	if err = PurgeStorage(context.Background(), fx.srv.configDir, fx.srv.stateDir, []string{fx.a.ID}, nil); err != nil {
		t.Fatal(err)
	}
	// Without a writer guard, search_path falls through to public after DROP
	// SCHEMA, so an already-open pool can create storage outside the catalog.
	if _, err = stale.Exec(`CREATE TABLE must_not_reappear(id BIGINT PRIMARY KEY)`); !errors.Is(err, pgdialect.ErrSchemaGone) {
		t.Fatal("stale pool did not refuse missing schema", err)
	}
	// A stale reader is refused too: every borrow checks its schema, never public.
	if err = stale.QueryRow(`SELECT 1`).Scan(new(int)); !errors.Is(err, pgdialect.ErrSchemaGone) {
		t.Fatal("stale pool read past the missing schema", err)
	}
	tx, err := dbbackend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err = tx.QueryRow(`SELECT to_regclass('public.must_not_reappear') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("stale writer escaped to public", exists, err)
	}
}

func TestTablePurgeFinalCheckRequiresDirectoryTombstones(t *testing.T) {
	dbbackend.FreshDatabaseForTest(t)
	px := newPurgeFixture(t)
	s := px.srv
	mountServerDocuments(t, s)
	px.purgeOrgA(t, 0)
	px.assertPurged(t)
	snap := tenancy.PurgeSnapshot{Users: []string{px.a.ID}, Mailboxes: []string{px.a.ID, px.a.extra}}
	gone := func(string) (bool, error) { return true, nil }
	left, err := s.purgeLeft(context.Background(), snap, gone)
	if err != nil || left["schemas"] != 0 {
		t.Fatal("completed purge lacks markers", left, err)
	}
	dir := s.userStateDir(px.a.ID)
	_, rel, err := dbbackend.SchemaName(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := dbbackend.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM kypost_catalog.stores WHERE path=? AND kind='purged'`, strings.TrimSuffix(rel, "x.db")); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	left, err = s.purgeLeft(context.Background(), snap, gone)
	if err != nil || left["schemas"] != 1 {
		t.Fatal("final check ignored missing marker", left, err)
	}
	if err = dbbackend.TombstoneDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}

func TestTableMailboxImportPausedPastPurgeFails(t *testing.T) {
	dbbackend.FreshDatabaseForTest(t)
	px := newPurgeFixture(t)
	s := px.srv
	mountServerDocuments(t, s)
	a, ok, err := s.ssoLifecycle.NativeMailboxAssignment(px.a.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	held, err := mailbox.OpenExisting(filepath.Join(a.Dir(s.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	resume := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-resume
		_, err := held.Import(context.Background(), mailbox.Receipt{Gateway: "held-importer", Delivery: "after-purge", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, strings.NewReader("From: sender@outside.test\r\nTo: "+a.Address+"\r\nSubject: held bytes\r\n\r\nretained input"))
		done <- err
	}()
	px.purgeOrgA(t, 0)
	close(resume)
	if err = <-done; !errors.Is(err, pgdialect.ErrSchemaGone) {
		t.Fatal("paused import was not refused at the store writer", err)
	}
	if held, err := dbbackend.DirectoryTombstoned(context.Background(), a.Dir(s.stateDir)); err != nil || !held {
		t.Fatal("mailbox directory is not tombstoned", held, err)
	}
	px.assertPurged(t)
}
