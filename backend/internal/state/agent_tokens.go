package state

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// Agent token scopes. The scope lives in the row, never in the token, so a
// token cannot claim one.
const (
	// AgentScopeRead reads mail metadata, unencrypted bodies, folders and labels.
	AgentScopeRead = "mcp:read"
	// AgentScopeOrganize adds labelling, moving, archiving and saving drafts.
	AgentScopeOrganize = "mcp:organize"
)

// IsAgentScope reports whether scope is one an agent token may carry.
func IsAgentScope(scope string) bool {
	return scope == AgentScopeRead || scope == AgentScopeOrganize
}

var (
	ErrAgentTokenNotFound = errors.New("agent token not found")
	ErrAgentTokenLimit    = errors.New("agent token limit reached")
)

// AgentToken is one /mcp credential. Times are unix seconds; LastUsedAt is 0
// until the token is first presented.
type AgentToken struct {
	ID         string
	UserID     string
	Label      string
	Hash       string
	Scope      string
	CreatedAt  int64
	LastUsedAt int64
}

// LastActive is when t was last used, or created if never used.
func (t AgentToken) LastActive() int64 {
	if t.LastUsedAt > 0 {
		return t.LastUsedAt
	}
	return t.CreatedAt
}

const agentTokenColumns = `id,user_id,label,hash,scope,created_at,last_used_at`

func scanAgentToken(row interface{ Scan(...any) error }) (AgentToken, error) {
	var t AgentToken
	err := row.Scan(&t.ID, &t.UserID, &t.Label, &t.Hash, &t.Scope, &t.CreatedAt, &t.LastUsedAt)
	return t, err
}

// CreateAgentToken stores t unless its user already holds max tokens; the
// count and the insert are one statement, so concurrent creates cannot both fit.
func (s *Store) CreateAgentToken(ctx context.Context, t AgentToken, max int) error {
	if !IsAgentScope(t.Scope) {
		return errors.New("unknown agent token scope")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO agent_tokens(`+agentTokenColumns+`)
		SELECT ?,?,?,?,?,?,0 WHERE (SELECT count(*) FROM agent_tokens WHERE user_id=?) < ?`,
		t.ID, t.UserID, t.Label, t.Hash, t.Scope, t.CreatedAt, t.UserID, max)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	return ErrAgentTokenLimit
}

// AgentTokens lists userID's tokens, oldest first.
func (s *Store) AgentTokens(ctx context.Context, userID string) ([]AgentToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentTokenColumns+` FROM agent_tokens WHERE user_id=? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentToken{}
	for rows.Next() {
		t, err := scanAgentToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AgentToken looks one token up by ID.
func (s *Store) AgentToken(ctx context.Context, id string) (AgentToken, error) {
	t, err := scanAgentToken(s.db.QueryRowContext(ctx, `SELECT `+agentTokenColumns+` FROM agent_tokens WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentToken{}, ErrAgentTokenNotFound
	}
	return t, err
}

// SetAgentTokenScope changes the scope of userID's token id.
func (s *Store) SetAgentTokenScope(ctx context.Context, userID, id, scope string) error {
	if !IsAgentScope(scope) {
		return errors.New("unknown agent token scope")
	}
	return s.oneAgentToken(s.db.ExecContext(ctx, `UPDATE agent_tokens SET scope=? WHERE id=? AND user_id=?`, scope, id, userID))
}

// DeleteAgentToken revokes userID's token id.
func (s *Store) DeleteAgentToken(ctx context.Context, userID, id string) error {
	return s.oneAgentToken(s.db.ExecContext(ctx, `DELETE FROM agent_tokens WHERE id=? AND user_id=?`, id, userID))
}

func (s *Store) oneAgentToken(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	return ErrAgentTokenNotFound
}

// DeleteUserAgentTokens revokes every token userID holds.
func (s *Store) DeleteUserAgentTokens(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_tokens WHERE user_id=?`, userID)
	return err
}

// TouchAgentToken records a use of id at now.
func (s *Store) TouchAgentToken(ctx context.Context, id string, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at=? WHERE id=?`, now, id)
	return err
}

// ExpireIdleAgentTokens deletes tokens last active at or before cutoff and
// returns them; a non-empty id limits it to that one token.
func (s *Store) ExpireIdleAgentTokens(ctx context.Context, cutoff int64, id string) ([]AgentToken, error) {
	rows, err := s.db.QueryContext(ctx, `DELETE FROM agent_tokens
		WHERE (CASE WHEN last_used_at>0 THEN last_used_at ELSE created_at END) <= ? AND (?='' OR id=?)
		RETURNING `+agentTokenColumns, cutoff, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gone []AgentToken
	for rows.Next() {
		t, err := scanAgentToken(rows)
		if err != nil {
			return nil, err
		}
		gone = append(gone, t)
	}
	return gone, rows.Err()
}

// FenceRestoredAgentTokens runs on stopped restore staging and revokes every
// agent token: the backup may hold tokens the user had already revoked, and an
// agent reconnects with a fresh token after a restore.
func FenceRestoredAgentTokens(stateDir string) (err error) {
	absolute, err := filepath.Abs(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absolute); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolute}).String()+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='agent_tokens'").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	_, err = db.Exec(`DELETE FROM agent_tokens`)
	return err
}
