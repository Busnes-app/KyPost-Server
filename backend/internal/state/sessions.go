package state

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Shared sessions for PostgreSQL replicas, in the global state.db. A session row is keyed by
// the SHA-256 of its token, never the token; data is the API's own encoding of the session.
// Every request reads the row, so a logout or revocation on one replica is seen by every
// other on its next request.

// ErrSessionFenced refuses a session whose sign-in began before a revocation or SSO trust
// change that has since happened.
var ErrSessionFenced = errors.New("state: credentials or sign-in settings changed during sign-in")

const (
	metaSSOEpoch      = "sso_epoch"
	metaRevocationSeq = "session_revocation_seq"
)

// SessionFence is what a sign-in read before checking the credential: the user's revocation
// generation, and, for an SSO sign-in, the SSO epoch.
type SessionFence struct {
	Revocations uint64 // refuse if the user's generation has moved past it
	CheckSSO    bool
	SSOEpoch    uint64 // refuse if the epoch differs (CheckSSO)
}

// PutSession records a new session, refusing it if fence no longer holds. The check and the
// insert share one write transaction, which serialises against RevokeUserSessions and
// EndSSOSessions.
func (s *Store) PutSession(ctx context.Context, tokenHash, userID, ssoSubject, data string, expiresMs int64, fence SessionFence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	gen, err := revocationGen(ctx, tx, userID)
	if err != nil {
		return err
	}
	if gen > fence.Revocations {
		return ErrSessionFenced
	}
	if fence.CheckSSO {
		epoch, err := ssoEpoch(ctx, tx)
		if err != nil {
			return err
		}
		if epoch != fence.SSOEpoch {
			return ErrSessionFenced
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, sso_subject, data, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, userID, ssoSubject, data, expiresMs); err != nil {
		return err
	}
	return tx.Commit()
}

// Session reads a live session's data.
func (s *Store) Session(ctx context.Context, tokenHash string) (data string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT data FROM sessions WHERE token_hash = ? AND expires_at > `+nowMillis(), tokenHash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return data, err == nil, err
}

// UpdateSession rewrites a session's data if it still holds old and returns whether it did:
// a compare-and-swap, so a one-time grant on the session is spent once across replicas.
func (s *Store) UpdateSession(ctx context.Context, tokenHash, old, data string, expiresMs int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET data = ?, expires_at = ? WHERE token_hash = ? AND data = ?`, data, expiresMs, tokenHash, old)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteSession ends one session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// RevokeUserSessions stamps userID with the next value of the global revocation sequence and
// deletes every session of the user but keepHash, in one transaction, and returns the stamp.
// A sign-in reads RevocationSeq before checking the credential; a user stamped past it was
// revoked since.
func (s *Store) RevokeUserSessions(ctx context.Context, userID, keepHash string) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := metaUint(ctx, tx, metaRevocationSeq)
	if err != nil {
		return 0, err
	}
	seq++
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaRevocationSeq, strconv.FormatUint(seq, 10)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_revocations (user_id, gen) VALUES (?, ?)
ON CONFLICT (user_id) DO UPDATE SET gen = excluded.gen`, userID, seq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash); err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// RevocationSeq is the global revocation sequence: the fence a sign-in reads first.
func (s *Store) RevocationSeq(ctx context.Context) (uint64, error) {
	return metaUint(ctx, s.db, metaRevocationSeq)
}

// RevocationGen is userID's revocation generation (0 before the first).
func (s *Store) RevocationGen(ctx context.Context, userID string) (uint64, error) {
	return revocationGen(ctx, s.db, userID)
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func revocationGen(ctx context.Context, q rowQueryer, userID string) (uint64, error) {
	var gen uint64
	err := q.QueryRowContext(ctx, `SELECT gen FROM session_revocations WHERE user_id = ?`, userID).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return gen, err
}

// SSOEpoch counts SSO trust changes (EndSSOSessions).
func (s *Store) SSOEpoch(ctx context.Context) (uint64, error) { return ssoEpoch(ctx, s.db) }

func ssoEpoch(ctx context.Context, q rowQueryer) (uint64, error) {
	return metaUint(ctx, q, metaSSOEpoch)
}

func metaUint(ctx context.Context, q rowQueryer, key string) (uint64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}

// EndSSOSessions bumps the SSO epoch and deletes every SSO session in one transaction, so a
// sign-in already past its settings read cannot mint one after.
func (s *Store) EndSSOSessions(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	epoch, err := ssoEpoch(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaSSOEpoch, strconv.FormatUint(epoch+1, 10)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE sso_subject <> ''`); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSSOSessionsWhere deletes every SSO session whose data covered reports true and returns
// how many, in one transaction.
func (s *Store) DeleteSSOSessionsWhere(ctx context.Context, covered func(data string) bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT token_hash, data FROM sessions WHERE sso_subject <> ''`)
	if err != nil {
		return 0, err
	}
	var doomed []string
	for rows.Next() {
		var hash, data string
		if err := rows.Scan(&hash, &data); err != nil {
			rows.Close()
			return 0, err
		}
		if covered(data) {
			doomed = append(doomed, hash)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, h := range doomed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, h); err != nil {
			return 0, err
		}
	}
	return len(doomed), tx.Commit()
}
