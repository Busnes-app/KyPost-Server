package state

import "context"

// The root store's tables keyed by an account's user_id (tenancy design, Store inventory,
// class D): the org purge deletes, then counts, their rows.
var (
	purgeDeletes = []string{`DELETE FROM agent_tokens WHERE user_id = ?`, `DELETE FROM scheduled_sends WHERE user_id = ?`,
		`DELETE FROM send_ledger WHERE user_id = ?`, `DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM session_revocations WHERE user_id = ?`, `DELETE FROM auth_challenges WHERE user_id = ?`,
		`DELETE FROM import_jobs WHERE user_id = ?`}
	purgeCount = `SELECT (SELECT count(*) FROM agent_tokens WHERE user_id = ?) + (SELECT count(*) FROM scheduled_sends WHERE user_id = ?)
 + (SELECT count(*) FROM send_ledger WHERE user_id = ?) + (SELECT count(*) FROM sessions WHERE user_id = ?)
 + (SELECT count(*) FROM session_revocations WHERE user_id = ?) + (SELECT count(*) FROM auth_challenges WHERE user_id = ?)
 + (SELECT count(*) FROM import_jobs WHERE user_id = ?)`
)

// PurgeUsers deletes every root-store row of the accounts, in one transaction. Multi-tenant
// only (the org purge).
func (s *Store) PurgeUsers(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		for _, q := range purgeDeletes {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// DeleteUserScheduledSends drops the account's scheduled sends without sending them.
func (s *Store) DeleteUserScheduledSends(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_sends WHERE user_id = ?`, userID)
	return err
}

// UserRows counts the accounts' rows in the tables PurgeUsers deletes from.
func (s *Store) UserRows(ctx context.Context, ids []string) (int, error) {
	total := 0
	for _, id := range ids {
		var n int
		if err := s.db.QueryRowContext(ctx, purgeCount, id, id, id, id, id, id, id).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
