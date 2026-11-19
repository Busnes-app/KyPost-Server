package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Shared security state for PostgreSQL replicas. NewServer keeps everything in process
// memory, as one SQLite container needs; app wiring calls UseSharedState on PostgreSQL, and
// from then on sessions, lockouts, the login KDF budget, office budgets, one-time authority
// and replay guards live in the global state.db, on database time. Every read goes to the
// database: there is no cross-replica cache. A store failure denies or answers 503, never
// falls back to memory and never reads as "signed out".

// sharedTimeout bounds one coordination statement outside a request's own deadline (detached
// refunds, the long routes). Generous: under load a tight bound turns sign-ins into 503s.
const sharedTimeout = 5 * time.Second

// errSharedUnavailable is a coordination store that could not answer.
var errSharedUnavailable = errors.New("shared security state unavailable")

// UseSharedState moves this server's security state into st, the global state store. Call
// once, before Serve, on PostgreSQL only.
func (s *Server) UseSharedState(st *state.Store) {
	s.shared = st
	s.mfaChallenges.Share(st)
	s.singleUse.shared = st
	s.ssoStates.shared = st
	if s.powVerifier != nil {
		s.powVerifier.SetSpender(st.SpendChallenge)
	}
	for name, l := range map[string]*failureLockout{
		"login": s.loginLockout, "review-pairing": s.reviewPairingLockout, "login-ip": s.loginIPLockout,
		"dav": s.davLockout, "mcp-ip": s.mcpLockout, "mcp-user": s.mcpUserLockout, "mfa": s.mfaLockout,
		"password-change": s.passwordChangeLockout, "imap-login": s.imapLoginLockout, "device": s.deviceLockout,
	} {
		l.shared, l.name = st, "lockout:"+name
	}
	for name, l := range map[string]*keyedLimiter{
		"login-kdf": s.loginRateLimiter, "office-pow": s.officePoW,
		"office-login-params": s.officeLoginParams, "office-login": s.officeLogin,
	} {
		l.shared, l.name = st, "budget:"+name
	}
}

// sharedKey is how a lockout or budget key is stored: hashed, so no username, address or
// NUL separator reaches the table, and every key is one length.
func sharedKey(name, key string) string {
	sum := sha256.Sum256([]byte(key))
	return name + ":" + hex.EncodeToString(sum[:])
}

func sharedContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), sharedTimeout)
}

// takeShared counts one hit of key in a shared lockout. A store failure denies.
func (l *failureLockout) takeShared(key string) (bool, time.Duration, lockoutReservation) {
	ctx, cancel := sharedContext()
	defer cancel()
	gen, left, ok, err := l.shared.TakeAttempt(ctx, sharedKey(l.name, key), l.maxFailures, l.lockoutFor)
	if err != nil {
		return false, sharedTimeout, lockoutReservation{unavailable: true}
	}
	if !ok {
		return false, left, lockoutReservation{}
	}
	return true, 0, lockoutReservation{key: key, gen: gen}
}

func (l *failureLockout) releaseShared(res lockoutReservation) {
	if res.gen == 0 {
		return
	}
	ctx, cancel := sharedContext()
	defer cancel()
	_ = l.shared.ReleaseAttempt(ctx, sharedKey(l.name, res.key), l.maxFailures, res.gen)
}

func (l *failureLockout) lockedForShared(key string) time.Duration {
	ctx, cancel := sharedContext()
	defer cancel()
	hits, left, err := l.shared.AttemptHits(ctx, sharedKey(l.name, key))
	if err != nil {
		return sharedTimeout
	}
	if hits >= l.maxFailures {
		return left
	}
	return 0
}

func (l *failureLockout) clearShared(key string) {
	ctx, cancel := sharedContext()
	defer cancel()
	_ = l.shared.ClearAttempts(ctx, sharedKey(l.name, key))
}

// sharedWindow is a token bucket as a database window: burst hits per burst/refill seconds,
// the same long-run rate. cost scales the limit to reservations of that size.
func (l *keyedLimiter) sharedWindow(cost float64) (limit int, window time.Duration) {
	limit = max(int(math.Floor(l.burst/cost)), 1)
	window = time.Duration(l.burst / l.refillPerSec * float64(time.Second))
	return limit, window
}

// take counts one reservation of cost in key's shared window and returns its generation for
// give. A store failure denies.
func (l *keyedLimiter) take(key string, cost float64) (ok bool, retryAfter time.Duration, gen int64) {
	ctx, cancel := sharedContext()
	defer cancel()
	limit, window := l.sharedWindow(cost)
	gen, left, ok, err := l.shared.TakeAttempt(ctx, sharedKey(l.name, key), limit, window)
	if err != nil {
		return false, sharedTimeout, 0
	}
	if !ok {
		return false, left, 0
	}
	return true, 0, gen
}

// reserve is admitCost on either backend; gen is the shared window's generation (0 in
// memory, where the reservation's cost settles through settleCost instead).
func (l *keyedLimiter) reserve(key string, cost float64) (ok bool, retryAfter time.Duration, gen int64) {
	if l.shared != nil {
		return l.take(key, cost)
	}
	ok, retryAfter = l.admitCost(key, cost)
	return ok, retryAfter, 0
}

// give hands one reservation back to the window gen, never to a newer one.
func (l *keyedLimiter) give(key string, cost float64, gen int64) {
	if gen == 0 {
		return
	}
	ctx, cancel := sharedContext()
	defer cancel()
	limit, _ := l.sharedWindow(cost)
	_ = l.shared.ReleaseAttempt(ctx, sharedKey(l.name, key), limit, gen)
}

// Sessions.

// sessionHash is how a token is stored: its SHA-256, never the token.
func sessionHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func encodeSession(sess Session) (string, error) {
	b, err := json.Marshal(sess)
	return string(b), err
}

// lookupSession reads the session behind token. ok false with a nil error is "no session";
// an error is "unknown", which callers answer with 503, never 401.
func (s *Server) lookupSession(ctx context.Context, token string) (Session, bool, error) {
	if s.shared == nil {
		s.sessMu.RLock()
		sess, ok := s.sessions[token]
		s.sessMu.RUnlock()
		return sess, ok, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	data, ok, err := s.shared.Session(ctx, sessionHash(token))
	if err != nil || !ok {
		return Session{}, false, err
	}
	var sess Session
	if err := json.Unmarshal([]byte(data), &sess); err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// changeSession applies change to token's session if it still exists, and reports whether it
// was written. change reports whether to write. On PostgreSQL the write is a compare-and-swap
// on the session as read: a concurrent change on another replica makes it false, so a grant
// change consumes is spent once.
func (s *Server) changeSession(ctx context.Context, token string, change func(*Session) bool) (bool, error) {
	if s.shared == nil {
		s.sessMu.Lock()
		defer s.sessMu.Unlock()
		sess, ok := s.sessions[token]
		if !ok || !change(&sess) {
			return false, nil
		}
		s.sessions[token] = sess
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	old, ok, err := s.shared.Session(ctx, sessionHash(token))
	if err != nil || !ok {
		return false, err
	}
	var sess Session
	if err := json.Unmarshal([]byte(old), &sess); err != nil {
		return false, err
	}
	if !change(&sess) {
		return false, nil
	}
	data, err := encodeSession(sess)
	if err != nil {
		return false, err
	}
	return s.shared.UpdateSession(ctx, sessionHash(token), old, data, sess.ExpiresAt.UnixMilli())
}

// dropSession ends token's session.
func (s *Server) dropSession(ctx context.Context, token string) error {
	if s.shared == nil {
		s.sessMu.Lock()
		delete(s.sessions, token)
		s.sessMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	return s.shared.DeleteSession(ctx, sessionHash(token))
}

// revocationMark is read before a sign-in checks the credential; revokedSince reports a
// revocation of userID after it.
func (s *Server) revocationMark(ctx context.Context) (uint64, error) {
	if s.shared == nil {
		return s.revocations.Load(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	return s.shared.RevocationSeq(ctx)
}

func (s *Server) revokedSince(ctx context.Context, userID string, mark uint64) (bool, error) {
	if s.shared == nil {
		s.sessMu.RLock()
		defer s.sessMu.RUnlock()
		return s.revokedAt[userID] >= mark+1, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	gen, err := s.shared.RevocationGen(ctx, userID)
	return gen > mark, err
}

// currentSSOEpoch counts SSO trust changes.
func (s *Server) currentSSOEpoch(ctx context.Context) (uint64, error) {
	if s.shared == nil {
		return s.ssoEpoch.Load(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, sharedTimeout)
	defer cancel()
	return s.shared.SSOEpoch(ctx)
}

// writeUnavailable is the answer when the session or security state cannot be read: the
// caller is not signed out, the service is.
func writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(sharedTimeout.Seconds())))
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "unavailable", "error": "KyPost is temporarily unavailable; you have not been signed out. Try again shortly."})
}
