package volumes

import (
	"context"
	"errors"
	"os"
)

// PurgeDocuments removes only the snapshot owners' documents. Account deletion moves
// before this operation when MH3 supplies the accounts-row guard.
func PurgeDocuments(ctx context.Context, stateDir string, owners []string) error {
	db, err := Open(stateDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback is cleanup on early return; after Commit it reports ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	for _, own := range owners {
		if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE owner = ?`, own); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeKey removes an org's wrapped key only after every document is removed. Member
// erasure deliberately keeps the org key so the other members' documents still open.
func PurgeKey(ctx context.Context, stateDir, org string) error {
	db, err := Open(stateDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM doc_keys WHERE org_id = ?`, org)
	return err
}

// KeyRows is the purge's final check for a whole-org scope.
func KeyRows(ctx context.Context, stateDir, org string) (int, error) {
	db, err := Open(stateDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM doc_keys WHERE org_id = ?`, org).Scan(&n)
	return n, err
}
