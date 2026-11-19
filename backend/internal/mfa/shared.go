package mfa

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Rows is the shared row store PostgreSQL replicas keep challenges in (state.Store's
// auth_challenges). Every transition is a compare-and-swap, so a challenge is completed on
// exactly one replica.
type Rows interface {
	PutChallenge(ctx context.Context, key, userID string, data []byte, expiresMs int64) error
	Challenge(ctx context.Context, key string) ([]byte, bool, error)
	SwapChallenge(ctx context.Context, key string, old, data []byte) (bool, error)
	TakeChallenge(ctx context.Context, key string, old []byte) (bool, error)
	UserChallenges(ctx context.Context, prefix, userID string) (map[string][]byte, error)
	DropUserChallenges(ctx context.Context, prefix, userID string) error
}

// Share moves the store's challenges into rows. Call once, before use.
func (s *Store) Share(rows Rows) { s.rows = rows }

const (
	rowPrefix  = "mfa:"
	rowTimeout = 5 * time.Second
	casTries   = 8
)

// rowKey is the row of challenge id: its hash, so the database never holds the ID a TOTP
// code completes.
func rowKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return rowPrefix + hex.EncodeToString(sum[:])
}

// secretDigest is how FinishSecret is stored: the browser holds the secret, the row a hash.
func secretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func rowContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), rowTimeout)
}

func encodeRow(ch Challenge) []byte {
	ch.ID = ""
	b, _ := json.Marshal(ch)
	return b
}

func (s *Store) putRow(ch Challenge) error {
	ctx, cancel := rowContext()
	defer cancel()
	stored := ch
	stored.FinishSecret = secretDigest(ch.FinishSecret)
	return s.rows.PutChallenge(ctx, rowKey(ch.ID), ch.UserID, encodeRow(stored), ch.ExpiresAt.UnixMilli())
}

// loadRow reads id's live challenge and its stored bytes. A store error reads as missing:
// the caller fails closed.
func (s *Store) loadRow(ctx context.Context, id string) (Challenge, []byte, bool) {
	data, ok, err := s.rows.Challenge(ctx, rowKey(id))
	if err != nil || !ok {
		return Challenge{}, nil, false
	}
	var ch Challenge
	if json.Unmarshal(data, &ch) != nil || time.Now().After(ch.ExpiresAt) {
		return Challenge{}, nil, false
	}
	ch.ID = id
	return ch, data, true
}

// errDropRow asks mutateRow to delete the row instead of rewriting it, then return the
// wrapped error.
type errDropRow struct{ err error }

func (e errDropRow) Error() string { return e.err.Error() }

// mutateRow applies change to id's challenge as a compare-and-swap, retrying when another
// replica changed it first. change returns nil to write, errDropRow to delete, or another
// error to leave the row and return it.
func (s *Store) mutateRow(id string, change func(*Challenge) error) error {
	ctx, cancel := rowContext()
	defer cancel()
	for range casTries {
		ch, old, ok := s.loadRow(ctx, id)
		if !ok {
			return ErrChallengeNotFound
		}
		err := change(&ch)
		var drop errDropRow
		switch {
		case errors.As(err, &drop):
			if took, terr := s.rows.TakeChallenge(ctx, rowKey(id), old); terr == nil && took {
				return drop.err
			}
		case err != nil:
			return err
		default:
			if swapped, serr := s.rows.SwapChallenge(ctx, rowKey(id), old, encodeRow(ch)); serr == nil && swapped {
				return nil
			}
		}
	}
	return ErrChallengeNotFound
}

func (s *Store) consumePushRow(id, secret string) (string, error) {
	ctx, cancel := rowContext()
	defer cancel()
	ch, old, ok := s.loadRow(ctx, id)
	if !ok {
		return "", ErrChallengeNotFound
	}
	if subtle.ConstantTimeCompare([]byte(secretDigest(secret)), []byte(ch.FinishSecret)) != 1 {
		return "", ErrChallengeNotFound
	}
	if ch.PushStatus != PushApproved {
		return "", ErrPushNotApproved
	}
	if took, err := s.rows.TakeChallenge(ctx, rowKey(id), old); err != nil || !took {
		return "", ErrChallengeNotFound
	}
	return ch.UserID, nil
}

func (s *Store) supersedeRows(userID, exceptID string) int {
	ctx, cancel := rowContext()
	defer cancel()
	rows, err := s.rows.UserChallenges(ctx, rowPrefix, userID)
	if err != nil {
		return 0
	}
	n := 0
	except := ""
	if exceptID != "" {
		except = rowKey(exceptID)
	}
	for key, data := range rows {
		var ch Challenge
		if key == except || json.Unmarshal(data, &ch) != nil || isPushResolved(ch.PushStatus) {
			continue
		}
		if took, err := s.rows.TakeChallenge(ctx, key, data); err == nil && took {
			n++
		}
	}
	return n
}
