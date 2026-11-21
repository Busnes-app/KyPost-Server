package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

// Organisation people for recipient autocomplete (docs/SUITE-PEOPLE.md).
const (
	suitePeopleTTL   = time.Minute
	suitePeopleLimit = 10
	// suitePeopleMax bounds the cache; it is cleared whole when full.
	suitePeopleMax = 2000
)

type suitePerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type suitePeopleEntry struct {
	dir    suitedir.Client
	people []suitePerson
	until  time.Time
}

// suitePeopleCache holds answers per user, subject and query.
type suitePeopleCache struct {
	mu sync.Mutex
	m  map[string]suitePeopleEntry
}

// handleSuitePeople answers GET /api/suite/people?q=: people in the
// organisation matching q, from KyIdentity's suite directory under the
// caller's linked subject. Always 200 with a list; empty on any failure.
func (s *Server) handleSuitePeople(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if ok, wait := s.peopleLimiter.allow(ac.UserID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many searches; try again shortly", "code": "rate_limited"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]suitePerson{"people": s.suitePeopleFor(r.Context(), ac.UserID, r.URL.Query().Get("q"))})
}

func (s *Server) suitePeopleFor(ctx context.Context, userID, q string) []suitePerson {
	none := []suitePerson{}
	q = strings.TrimSpace(q)
	if n := utf8.RuneCountInString(q); n < 2 || n > 64 || !utf8.ValidString(q) {
		return none
	}
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if !ok {
		return none
	}
	u, err := s.users.Get(userID)
	if err != nil || !u.Active || !linkedTo(u, dir.Issuer) {
		return none
	}
	key := userID + "\x00" + u.SSOSub + "\x00" + strings.ToLower(q)
	s.suitePeople.mu.Lock()
	e, hit := s.suitePeople.m[key]
	s.suitePeople.mu.Unlock()
	if hit && e.dir == dir && time.Now().Before(e.until) {
		return e.people
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	found, err := dir.People(tctx, u.SSOSub, q, suitePeopleLimit)
	cancel()
	found = s.sameOrgPeople(ctx, u, dir.Issuer, found)
	if err != nil {
		// suitedir errors carry neither the query nor the credentials.
		s.logger.Warn("suite people unavailable", "user_id", userID, "error", err.Error())
		found = nil
	}
	people := none
	for _, p := range found {
		people = append(people, suitePerson{Name: p.Name, Email: p.Email})
	}
	// A request the browser abandoned says nothing about KyIdentity: do not cache it.
	if ctx.Err() == nil {
		s.suitePeople.mu.Lock()
		if s.suitePeople.m == nil || len(s.suitePeople.m) >= suitePeopleMax {
			s.suitePeople.m = map[string]suitePeopleEntry{}
		}
		s.suitePeople.m[key] = suitePeopleEntry{dir: dir, people: people, until: time.Now().Add(suitePeopleTTL)}
		s.suitePeople.mu.Unlock()
	}
	return people
}
