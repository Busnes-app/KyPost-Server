package api

import (
	"context"
	"encoding/json"
	"time"
)

// One-time authority as rows of the global state store's auth_challenges on PostgreSQL. A row
// is keyed by prefix plus the hash of its secret ID and owned by an owner string (a user ID
// or a session hash); every edit is a compare-and-swap on the row's bytes, so each consume or
// completion succeeds on exactly one replica.

// rowEdit is what an edit does to the row it read.
type rowEdit int

const (
	rowKeep  rowEdit = iota // leave it
	rowWrite                // replace it with the edited value
	rowDrop                 // delete it
)

const rowTries = 8

func rowKeyOf(prefix, id string) string { return prefix + ":" + sessionHash(id) }

// editRow applies edit to the row at prefix/id as a compare-and-swap and returns edit's
// verdict; a lost race reads again. found is false for a missing or expired row (or one the
// store could not read, which fails closed).
func editRow[T any](st sharedRows, prefix, id string, edit func(v *T, found bool) (rowEdit, bool)) bool {
	ctx, cancel := sharedContext()
	defer cancel()
	key := rowKeyOf(prefix, id)
	for range rowTries {
		var v T
		old, found, err := st.Challenge(ctx, key)
		if err == nil && found && json.Unmarshal(old, &v) != nil {
			found = false
		}
		action, verdict := edit(&v, err == nil && found)
		switch action {
		case rowKeep:
			return verdict
		case rowDrop:
			if took, err := st.TakeChallenge(ctx, key, old); err == nil && took {
				return verdict
			}
		case rowWrite:
			data, _ := json.Marshal(v)
			if swapped, err := st.SwapChallenge(ctx, key, old, data); err == nil && swapped {
				return verdict
			}
		}
		if err != nil || !found {
			return false
		}
	}
	return false
}

// putRow records v at prefix/id for owner until expires, first dropping owner's other rows
// of prefix when single is set.
func putRow(st sharedRows, prefix, id, owner string, v any, expires time.Time, single bool) error {
	ctx, cancel := sharedContext()
	defer cancel()
	if single {
		if err := st.DropUserChallenges(ctx, prefix+":", owner); err != nil {
			return err
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.PutChallenge(ctx, rowKeyOf(prefix, id), owner, data, expires.UnixMilli())
}

// sharedRows is the part of state.Store rows need.
type sharedRows interface {
	PutChallenge(ctx context.Context, key, userID string, data []byte, expiresMs int64) error
	Challenge(ctx context.Context, key string) ([]byte, bool, error)
	SwapChallenge(ctx context.Context, key string, old, data []byte) (bool, error)
	TakeChallenge(ctx context.Context, key string, old []byte) (bool, error)
	DropUserChallenges(ctx context.Context, prefix, userID string) error
}

// editStepUp applies edit to step-up id atomically: under stepUpMu in memory, as a
// compare-and-swap on PostgreSQL.
func (s *Server) editStepUp(id string, edit func(c *ssoStepUp, found bool) (rowEdit, bool)) bool {
	if s.shared != nil {
		return editRow(s.shared, "stepup", id, edit)
	}
	s.stepUpMu.Lock()
	defer s.stepUpMu.Unlock()
	c, found := s.stepUps[id]
	action, verdict := edit(&c, found)
	switch action {
	case rowWrite:
		s.stepUps[id] = c
	case rowDrop:
		delete(s.stepUps, id)
	}
	return verdict
}

// putStepUp records a new step-up challenge, the only live one of its session.
func (s *Server) putStepUp(id string, c ssoStepUp) error {
	if s.shared != nil {
		return putRow(s.shared, "stepup", id, c.Session, c, c.Expires, true)
	}
	s.stepUpMu.Lock()
	defer s.stepUpMu.Unlock()
	now := time.Now()
	// One live challenge per session, and nothing expired kept around.
	for k, old := range s.stepUps {
		if !now.Before(old.Expires) || old.Session == c.Session {
			delete(s.stepUps, k)
		}
	}
	s.stepUps[id] = c
	return nil
}
