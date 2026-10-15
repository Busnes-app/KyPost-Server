package api

import (
	"math"
	"sync"
	"time"
)

// keyedLimiterSweepThreshold bounds how large a limiter's map may grow before
// idle entries are reclaimed at insertion: a stream of distinct keys (source
// IPs, candidate addresses) would otherwise each leave an entry behind until
// the next periodic sweep. Buckets refilled to full carry no state, so they
// are the ones dropped.
const keyedLimiterSweepThreshold = 10_000

type rateBucket struct {
	tokens float64
	last   time.Time
}

// keyedLimiter is the one in-memory per-key token bucket: request rates (WKD,
// push polls, account writes, PoW challenges), cooldowns (burst 1), the MFA
// push cap and the instance-wide login budget (admitCost/settleCost, one
// constant key). Every decision is one call under one lock, so a burst of
// concurrent callers can never all see "allowed". Strikes are failureLockout's.
// It lives outside Server.mu: unrelated state with a much smaller lock scope.
type keyedLimiter struct {
	mu             sync.Mutex
	burst          float64
	refillPerSec   float64
	sweepThreshold int
	entries        map[string]*rateBucket
	// now is overridable in tests so refill behavior can be exercised
	// without sleeping.
	now func() time.Time
}

func newKeyedLimiter(burst, refillPerSec float64) *keyedLimiter {
	return &keyedLimiter{
		burst:          burst,
		refillPerSec:   refillPerSec,
		sweepThreshold: keyedLimiterSweepThreshold,
		entries:        map[string]*rateBucket{},
		now:            time.Now,
	}
}

// allow consumes one token for key, reporting whether the request may
// proceed. When it may not, retryAfter is how long until a token is
// available. A nil limiter allows everything.
func (l *keyedLimiter) allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.refillLocked(key, l.now())

	if e.tokens >= 1 {
		e.tokens--
		return true, 0
	}
	if l.refillPerSec <= 0 {
		return false, time.Second
	}
	return false, time.Duration((1-e.tokens)/l.refillPerSec*float64(time.Second)) + time.Millisecond
}

// refillLocked brings key's bucket up to date, creating it at full burst if it
// is new, and returns it. Callers must hold l.mu.
func (l *keyedLimiter) refillLocked(key string, now time.Time) *rateBucket {
	e, exists := l.entries[key]
	if !exists {
		if len(l.entries) >= l.sweepThreshold {
			l.sweepLocked(now)
		}
		e = &rateBucket{tokens: l.burst, last: now}
		l.entries[key] = e
		return e
	}
	// Refill for elapsed time, capped at burst so an idle client can't
	// accumulate an unbounded allowance.
	if elapsed := now.Sub(e.last).Seconds(); elapsed > 0 {
		e.tokens = math.Min(l.burst, e.tokens+elapsed*l.refillPerSec)
	}
	e.last = now
	return e
}

// admitCost is allow() for a bucket denominated in seconds of work rather than
// in requests, for callers that are about to do something whose true cost they
// cannot know until it is done.
//
// It takes reserve up front and admits on any positive balance, so a bucket may
// go into debt — that is deliberate. The alternative is to refuse work until a
// full attempt's worth of budget has accrued, which on a machine where an
// attempt costs far more than reserve would refuse everything forever. Debt lets
// the true cost land after the fact and simply delays the next admission until
// the deficit has refilled, which is the behaviour the budget describes.
//
// The debt is FLOORED at one full burst below zero. Without
// a floor, a caller who can make the reservation without paying it back drives
// the balance arbitrarily negative and the retryAfter handed to everybody else
// grows without bound — a few minutes of that is an hours-long outage that
// outlives the abuse by as long as it takes to refill. A floor caps the recovery
// time at burst/refillPerSec no matter how long the abuse ran. Every caller must
// still settle its own reservation; this only bounds the blast radius when one
// forgets.
func (l *keyedLimiter) admitCost(key string, reserve float64) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.refillLocked(key, l.now())

	if e.tokens > 0 {
		e.tokens = math.Max(e.tokens-reserve, -l.burst)
		return true, 0
	}
	if l.refillPerSec <= 0 {
		return false, time.Second
	}
	// Time to climb back out of debt and be admissible again.
	return false, time.Duration(-e.tokens/l.refillPerSec*float64(time.Second)) + time.Millisecond
}

// settleCost reconciles the reservation admitCost took against what the work
// actually cost. delta is (actual - reserve): positive bills the difference,
// negative refunds it.
//
// Clamped at both ends, to the same bounds admitCost uses: a refund cannot
// mint tokens above burst, and a bill cannot drive the balance further than
// one burst into debt. The floor forgives some genuinely-incurred cost when a
// single derivation runs pathologically long, and that is the right trade —
// an unbounded deficit turns one slow attempt into an outage measured in
// hours for every other caller of the bucket.
func (l *keyedLimiter) settleCost(key string, delta float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A swept bucket is recreated full, so a bill is never dropped.
	e := l.refillLocked(key, l.now())
	e.tokens = math.Max(math.Min(l.burst, e.tokens-delta), -l.burst)
}

// sweep is sweepLocked for the periodic sweepers.
func (l *keyedLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(l.now())
}

// sweepLocked drops entries that have refilled to full — an idle bucket is
// indistinguishable from one that was never created, so it holds no state
// worth the memory. Callers must hold l.mu.
func (l *keyedLimiter) sweepLocked(now time.Time) {
	for k, e := range l.entries {
		tokens := e.tokens
		if elapsed := now.Sub(e.last).Seconds(); elapsed > 0 {
			tokens = math.Min(l.burst, tokens+elapsed*l.refillPerSec)
		}
		if tokens >= l.burst {
			delete(l.entries, k)
		}
	}
}
