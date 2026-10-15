package api

import "time"

const (
	// powChallengeBurst/powChallengeWindowLen cap how many proof-of-work
	// challenges one client IP can mint. GET /api/auth/pow-challenge is
	// unauthenticated by necessity — it runs before anyone has typed a
	// password — so without a cap it is a free entropy-and-CPU faucet, and
	// every challenge it issues is a salt that can later occupy an entry in
	// the verifier's spent-salt cache.
	//
	// 30/minute is far above real use (a login page fetches one, plus one
	// per retry) and far below anything worth doing on purpose.
	powChallengeBurst     = 30
	powChallengeWindowLen = time.Minute
)

// newPowChallengeLimiter meters challenges per client IP (lockoutKeyForIP):
// powChallengeBurst at once, refilling powChallengeBurst per window. At most
// twice the burst in any one window, the same ceiling the fixed window had.
func newPowChallengeLimiter() *keyedLimiter {
	return newKeyedLimiter(powChallengeBurst, powChallengeBurst/powChallengeWindowLen.Seconds())
}
