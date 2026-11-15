package api

import (
	"sync"
	"time"
)

const (
	// issuedSSOStateTTL matches the state cookie's MaxAge.
	issuedSSOStateTTL = 5 * time.Minute
	issuedSSOStateCap = 10_000
)

// issuedSSOStates holds the states handleSSOLogin issued after charging the
// sign-in meter, so the callback redeeming one is not charged a second time.
// The cookie carrying a state is the client's to write, so only this record
// proves a state was paid for. Each is redeemable once.
type issuedSSOStates struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (s *issuedSSOStates) add(state string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	if len(s.at) >= issuedSSOStateCap {
		for k, t := range s.at {
			if now.Sub(t) > issuedSSOStateTTL {
				delete(s.at, k)
			}
		}
		if len(s.at) >= issuedSSOStateCap {
			return // its callback pays, as an unrecorded state does
		}
	}
	s.at[state] = now
}

// take reports whether state was issued and is unexpired, and forgets it.
func (s *issuedSSOStates) take(state string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[state]
	delete(s.at, state)
	return ok && now.Sub(t) <= issuedSSOStateTTL
}
