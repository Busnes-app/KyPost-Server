package state

import (
	"context"
	"database/sql"
	"errors"
)

// One-time authority shared by PostgreSQL replicas (auth_challenges): MFA login challenges,
// SSO step-ups and states, pending device approvals, export and import grants. The caller
// keys each row by a hash of its secret ID and keeps any secret inside hashed. Every
// transition is a compare-and-swap on the row's bytes, so a completion or consume succeeds
// on exactly one replica.

// PutChallenge records a new row.
func (s *Store) PutChallenge(ctx context.Context, key, userID string, data []byte, expiresMs int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_challenges (key, user_id, data, expires_at) VALUES (?, ?, ?, ?)`, key, userID, data, expiresMs)
	return err
}

// Challenge reads a live row.
func (s *Store) Challenge(ctx context.Context, key string) (data []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT data FROM auth_challenges WHERE key = ? AND expires_at > `+nowMillis(), key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}

// SwapChallenge replaces a live row's data if it still holds old.
func (s *Store) SwapChallenge(ctx context.Context, key string, old, data []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_challenges SET data = ? WHERE key = ? AND data = ? AND expires_at > `+nowMillis(), data, key, old)
	return oneRow(res, err)
}

// TakeChallenge deletes a live row if it still holds old (nil: whatever it holds) and
// reports whether this call deleted it.
func (s *Store) TakeChallenge(ctx context.Context, key string, old []byte) (bool, error) {
	if old == nil {
		res, err := s.db.ExecContext(ctx, `DELETE FROM auth_challenges WHERE key = ? AND expires_at > `+nowMillis(), key)
		return oneRow(res, err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_challenges WHERE key = ? AND data = ? AND expires_at > `+nowMillis(), key, old)
	return oneRow(res, err)
}

// DropChallenge deletes a row, live or not.
func (s *Store) DropChallenge(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_challenges WHERE key = ?`, key)
	return err
}

// UserChallenges lists userID's live rows whose key starts with prefix.
func (s *Store) UserChallenges(ctx context.Context, prefix, userID string) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, data FROM auth_challenges WHERE user_id = ? AND substr(key, 1, ?) = ? AND expires_at > `+nowMillis(), userID, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var d []byte
		if err := rows.Scan(&k, &d); err != nil {
			return nil, err
		}
		out[k] = d
	}
	return out, rows.Err()
}

// DropUserChallenges deletes every row of userID whose key starts with prefix.
func (s *Store) DropUserChallenges(ctx context.Context, prefix, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_challenges WHERE user_id = ? AND substr(key, 1, ?) = ?`, userID, len(prefix), prefix)
	return err
}

func oneRow(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
