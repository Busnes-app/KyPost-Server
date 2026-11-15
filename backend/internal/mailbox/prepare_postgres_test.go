package mailbox

import (
	"errors"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
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
