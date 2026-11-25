package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

const (
	// powEscalationFactor multiplies the proof-of-work search space per recent
	// failed login from a client IP; powMaxNumberCeiling caps it.
	//
	// The cap is load-bearing: uncapped, a persistent attacker drives
	// difficulty to a value nobody can solve, denying service to everyone
	// behind that address. Measured cost at the ceiling: ~4.8 s average and
	// ~9.5 s worst case under crypto.subtle on a desktop, 20-40 s on a phone.
	powEscalationFactor = 4
	powMaxNumberCeiling = 1_000_000

	// powEscalationDecay is how long a failure keeps counting. Client IPs
	// are shared (carrier-grade NAT, office egress, a household), so a
	// stranger's failures must stop costing an innocent neighbour reasonably
	// soon. It mirrors loginLockoutFor for the same reason.
	powEscalationDecay = 15 * time.Minute

	// powEscalationSweepThreshold bounds the per-IP map between StartPoWSweeper
	// ticks. Any failed login from any address inserts an entry with no
	// credential required, so an attacker presenting many source IPs (rotation,
	// or an influenced X-Forwarded-For chain under TRUST_PROXY_HEADERS=true)
	// accumulates freely between ticks. Crossing this triggers an inline sweep.
	powEscalationSweepThreshold = 10_000
	// maxPowEscalationAccountsPerIP bounds the per-address account map.
	//
	// powEscalationSweepThreshold guards only the OUTER map, and its check lives
	// inside the !exists branch, so it runs only when a new address arrives. A
	// single address rotating usernames therefore grew one entry's byAccount map
	// without any bound, on a key the caller chooses.
	//
	// An address that has failed against this many distinct accounts is already
	// at maximum escalation; tracking more of them changes no decision.
	maxPowEscalationAccountsPerIP = 256
)

// powEscalationEntry counts an address's recent failures per targeted account
// rather than as one total. Difficulty is the sum, so the honest user mistyping
// their own password sees flat-counter behaviour; the split exists so
// clearAccount can forgive one account without forgiving the others.
type powEscalationEntry struct {
	byAccount map[string]int
	expiresAt time.Time
}

func (e *powEscalationEntry) total() int {
	sum := 0
	for _, n := range e.byAccount {
		sum += n
	}
	return sum
}

// powEscalation raises the proof-of-work difficulty for a client IP with
// recent failed logins, so a correct password on the first try stays nearly
// free while repeated guessing from one address gets expensive fast.
//
// It depends on challenges being bound to the address that requested them
// (captcha.PoWVerifier signs the client IP in and Verify checks it). Without
// that binding an attacker fetches base-difficulty challenges from a clean
// address and spends them from an escalated one, and escalation then prices
// nothing but the honest user who mistyped. A mobile client whose address
// changes mid-solve gets captcha.ErrChallengeWrongClient, which handleLogin
// refunds the lockout strike for.
//
// It does not reuse loginLockout: that is keyed username+IP, and the challenge
// is issued before the user has typed a username.
//
// ponytail: the outer key is still the client IP, as a /64 for IPv6 (see
// lockoutKeyForIP). Ceiling: an attacker rotating across DIFFERENT prefixes gets
// base difficulty at each, and everyone behind one shared address shares one
// budget. Rotation inside a single IPv6 prefix no longer buys anything, which is
// the case that was free. Upgrade path: none planned — narrowing
// it needs an identifier that exists before the login form is filled in, and
// there isn't one. (Failures ARE now split per targeted account within an
// address, so a success no longer forgives guesses at other accounts — see
// clearAccount.)
type powEscalation struct {
	mu             sync.Mutex
	entries        map[string]*powEscalationEntry
	sweepThreshold int
	// shared (MH6, table mode) keeps the failures in PostgreSQL instead, one window per
	// address and account on database time, so every replica prices an address alike.
	shared *state.Store
}

// powSharedPrefix is where an address's windows live: its accounts' keys share it, so the
// difficulty is one range sum.
func powSharedPrefix(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return "meter:pow-escalation:" + hex.EncodeToString(sum[:]) + ":"
}

func powSharedKey(ip, account string) string {
	sum := sha256.Sum256([]byte(account))
	return powSharedPrefix(ip) + hex.EncodeToString(sum[:])
}

// powSharedHits caps a shared window's count: enough failures to reach the ceiling from any
// base, so a counter never grows past what changes a price.
const powSharedHits = 32

func newPowEscalation() *powEscalation {
	return &powEscalation{
		entries:        map[string]*powEscalationEntry{},
		sweepThreshold: powEscalationSweepThreshold,
	}
}

// recordFailure counts one failed login against ip for the account it targeted
// and refreshes its decay window. account must already be folded through
// users.NormalizeUsername by the caller, so the three spellings of one
// username share one budget (the lesson handleLogin's lockout key learned).
func (p *powEscalation) recordFailure(ip, account string, now time.Time) {
	if p.shared != nil {
		// A failure that cannot be counted is lost; the login lockouts still bound guessing.
		ctx, cancel := sharedContext()
		defer cancel()
		_, _, _, _ = p.shared.TakeAttempt(ctx, powSharedKey(ip, account), powSharedHits, powEscalationDecay)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	e, exists := p.entries[ip]
	if !exists || now.After(e.expiresAt) {
		if !exists && len(p.entries) >= p.sweepThreshold {
			// Only the !exists branch grows the map, so only it needs the
			// bound. sweepExpiredLocked, not sweepExpired: p.mu is already
			// held here and sync.Mutex is not reentrant.
			p.sweepExpiredLocked(now)
		}
		e = &powEscalationEntry{byAccount: map[string]int{}}
		p.entries[ip] = e
	}
	// The outer map is bounded by sweepThreshold above; the inner one needs its
	// own cap. One address rotating usernames only ever takes the `exists`
	// branch, which never consults that threshold, so byAccount grew without
	// limit on a caller-supplied key.
	//
	// Past the cap, stop admitting NEW accounts and keep counting the ones
	// already tracked: escalation for an address that is spraying usernames is
	// already at its ceiling, so the extra keys buy accuracy nobody can use.
	if _, tracked := e.byAccount[account]; tracked || len(e.byAccount) < maxPowEscalationAccountsPerIP {
		e.byAccount[account]++
	}
	// Extend the decay window from FIRST failure, not from the latest one.
	// Refreshing it on every failure meant an entry never expired while its
	// attacker kept working — so the sweep could not reclaim exactly the
	// entries that were growing.
	if e.expiresAt.IsZero() {
		e.expiresAt = now.Add(powEscalationDecay)
	}
}

// clearAccount forgives ip's failures against one account, called when a login
// for that account succeeds.
//
// Deliberately not "delete the whole entry": that would let an attacker holding
// any ordinary account on the instance spray guesses at another username, log
// in as themselves to reset the price, and repeat. Forgiveness is scoped to the
// account actually proved.
func (p *powEscalation) clearAccount(ip, account string) {
	if p.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		_ = p.shared.ClearAttempts(ctx, powSharedKey(ip, account))
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, exists := p.entries[ip]
	if !exists {
		return
	}
	delete(e.byAccount, account)
	if len(e.byAccount) == 0 {
		delete(p.entries, ip)
	}
}

// maxNumberFor returns the search space to issue to ip: base multiplied by
// powEscalationFactor once per recent failure, capped.
func (p *powEscalation) maxNumberFor(ip string, base int, now time.Time) int {
	failures, err := p.failures(ip, now)
	if err != nil {
		return powMaxNumberCeiling // unknown is priced as the worst, never as clean
	}
	maxNumber := base
	for i := 0; i < failures; i++ {
		// Check before multiplying: powMaxNumberCeiling is far below
		// math.MaxInt, but growing by 4x per failure reaches overflow in
		// well under a hundred iterations and an attacker chooses the count.
		if maxNumber >= powMaxNumberCeiling/powEscalationFactor {
			return powMaxNumberCeiling
		}
		maxNumber *= powEscalationFactor
	}
	if maxNumber > powMaxNumberCeiling {
		return powMaxNumberCeiling
	}
	return maxNumber
}

// failures is ip's recent failure count across its accounts.
func (p *powEscalation) failures(ip string, now time.Time) (int, error) {
	if p.shared != nil {
		ctx, cancel := sharedContext()
		defer cancel()
		return p.shared.AttemptHitsUnder(ctx, powSharedPrefix(ip))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, exists := p.entries[ip]
	if !exists || now.After(e.expiresAt) {
		return 0, nil
	}
	return e.total(), nil
}

// sweepExpired drops decayed entries. Fed by unauthenticated callers, so it
// needs a real sweep — see backend/AGENTS.md. Driven by StartPoWSweeper
// unconditionally, since handleLogin records failures whatever
// CAPTCHA_PROVIDER says. This and recordFailure's threshold sweep are two
// independent bounds: the ticker guarantees eventual reclamation at low
// traffic, the threshold caps worst-case memory between ticks.
func (p *powEscalation) sweepExpired(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepExpiredLocked(now)
}

// sweepExpiredLocked does the actual reclamation. Callers must hold p.mu.
// Factored out so recordFailure can trigger it without re-locking a mutex it
// already holds.
func (p *powEscalation) sweepExpiredLocked(now time.Time) {
	for ip, e := range p.entries {
		if now.After(e.expiresAt) {
			delete(p.entries, ip)
		}
	}
}
