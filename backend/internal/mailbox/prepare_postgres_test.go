package mailbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// AC8: on Postgres an account's state and mailbox schemas appear together; a failure
// before the commit leaves no catalog row and no schema, and a retry succeeds.
func TestPreparePostgresPublishesAtomically(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	injected := errors.New("injected before commit")
	publishSchemasStep = func() error { return injected }
	_, err := PrepareAccount(root, preparationOwner(), "one@example.test", preparationLimits())
	publishSchemasStep = func() error { return nil }
	if !errors.Is(err, ErrPreparation) {
		t.Fatalf("injected failure: %v", err)
	}
	if rows, schemas := dbbackend.CatalogUnderForTest(t, root); rows != 0 || schemas != 0 {
		t.Fatalf("a failed preparation left %d catalog rows and %d schemas", rows, schemas)
	}
	source, err := PrepareAccount(root, preparationOwner(), "one@example.test", preparationLimits())
	if err != nil || source == "" {
		t.Fatalf("retry: %v", err)
	}
	if rows, schemas := dbbackend.CatalogUnderForTest(t, root); rows != 2 || schemas != 2 {
		t.Fatalf("a prepared account has %d catalog rows and %d schemas, want 2 and 2", rows, schemas)
	}
	if _, err := ValidatePreparedAccount(root, preparationOwner(), "one@example.test", preparationLimits()); err != nil {
		t.Fatal(err)
	}
}

// MH7: in table mode, PrepareAccount, ValidatePreparedAccount, ConvergeLimits, and Open
// store native-mailbox.json in the documents table and create nothing on the host's disk.
func TestPrepareTableModeWritesNothingToHostDisk(t *testing.T) {
	dbbackend.RequirePostgres(t)
	stateRoot := t.TempDir()
	db, err := volumes.Open(stateRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, err := vol.NewTable(db, "state", vol.TableOptions{
		MasterKey: bytes.Repeat([]byte{7}, 32),
		Org:       func(context.Context, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	unmount, err := vol.MountFenced(stateRoot, table, func(ctx context.Context, _ string, _ bool) (context.Context, func(), error) {
		return ctx, func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unmount()

	owner := preparationOwner()
	dir := filepath.Join(stateRoot, "users", owner.Mailbox)
	limits := preparationLimits()

	// Phase 1 (before accounts row exists): PrepareAccountStorageContext creates
	// mailbox.db + state.db without writing native-mailbox.json or touching disk.
	stagedSource, err := PrepareAccountStorageContext(context.Background(), stateRoot, owner, "one@example.test", limits)
	if err != nil || stagedSource == "" {
		t.Fatalf("PrepareAccountStorageContext in table mode: source=%q err=%v", stagedSource, err)
	}
	if entries, err := os.ReadDir(stateRoot); err != nil || len(entries) != 0 {
		t.Fatalf("table-mode PrepareAccountStorageContext touched host disk: entries=%v err=%v", entries, err)
	}

	// Simulate PublishPreparedSSOUser committing the accounts row.
	if _, err := db.Exec(`INSERT INTO accounts (id, username, record) VALUES (?, ?, ?)`,
		owner.Mailbox, "one", `{"id":"`+owner.Mailbox+`","orgId":""}`); err != nil {
		t.Fatalf("seed accounts row: %v", err)
	}

	// Phase 2 (after accounts row commits): PrepareAccount writes native-mailbox.json
	// into the documents table (locking accounts FOR KEY SHARE) and returns the same source.
	source, err := PrepareAccount(stateRoot, owner, "one@example.test", limits)
	if err != nil || source != stagedSource {
		t.Fatalf("PrepareAccount in table mode: source=%q want=%q err=%v", source, stagedSource, err)
	}
	if entries, err := os.ReadDir(stateRoot); err != nil || len(entries) != 0 {
		t.Fatalf("table-mode PrepareAccount touched host disk: entries=%v err=%v", entries, err)
	}
	if _, err := vol.OpenDocument(filepath.Join(dir, preparationFile)).Read(); err != nil {
		t.Fatalf("native-mailbox.json missing from documents table: %v", err)
	}

	if got, err := ValidatePreparedAccount(stateRoot, owner, "one@example.test", limits); err != nil || got != source {
		t.Fatalf("ValidatePreparedAccount: got=%q want=%q err=%v", got, source, err)
	}
	if entries, err := os.ReadDir(stateRoot); err != nil || len(entries) != 0 {
		t.Fatalf("table-mode ValidatePreparedAccount touched host disk: entries=%v err=%v", entries, err)
	}

	raised := limits
	raised.PayloadBytes += 1 << 20
	changed, err := ConvergeLimits(context.Background(), dir, owner, raised)
	if err != nil || !changed {
		t.Fatalf("ConvergeLimits: changed=%v err=%v", changed, err)
	}
	if got, err := ValidatePreparedAccount(stateRoot, owner, "one@example.test", raised); err != nil || got != source {
		t.Fatalf("ValidatePreparedAccount after ConvergeLimits: got=%q want=%q err=%v", got, source, err)
	}
	if entries, err := os.ReadDir(stateRoot); err != nil || len(entries) != 0 {
		t.Fatalf("table-mode ConvergeLimits touched host disk: entries=%v err=%v", entries, err)
	}
	store, err := Open(filepath.Join(dir, "mailbox"), owner, raised)
	if err != nil {
		t.Fatalf("Open in table mode: %v", err)
	}
	_ = store.Close()
	if entries, err := os.ReadDir(stateRoot); err != nil || len(entries) != 0 {
		t.Fatalf("table-mode ConvergeLimits/Open touched host disk: entries=%v err=%v", entries, err)
	}
}
