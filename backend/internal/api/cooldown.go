package api

import "time"

// newCooldown is per-key "not again for every": a keyedLimiter with a burst of
// one that refills after every. allow both checks and starts the cooldown, in
// one critical section: the two-call form let a concurrent burst all see
// "allowed", and on the send-as path mailed a third party several times over.
func newCooldown(every time.Duration) *keyedLimiter {
	return newKeyedLimiter(1, 1/every.Seconds())
}

// sendAsVerificationCooldownFor bounds how often a send-as verification probe
// email may be dispatched for a given (user, candidate address) pair: without
// this, an authenticated user could repeatedly trigger probe emails at any
// third-party address, turning the endpoint into a spam/harassment vector
// against people who never asked to receive anything from this account. This
// does not block the underlying alias record's lifecycle — it only limits how
// often a *new* probe email goes out for the same pair.
//
// Keyed on userID+"|"+normalizedEmail (not bare userID) since the goal is to
// limit how often any single candidate address gets emailed, without penalizing
// a user who is concurrently verifying a different address of their own.
const sendAsVerificationCooldownFor = 5 * time.Minute

// notificationTestCooldownFor bounds how often one user may fire
// POST /api/notifications/test.
//
// The handler fans out to every registration the account has, serially, each
// with its own multi-second network timeout, on the request goroutine. It is
// the one endpoint that lets an authenticated user trigger that fanout on
// demand and as often as they like, against destinations they chose. Keyed on
// the caller's user ID.
const notificationTestCooldownFor = 10 * time.Second
