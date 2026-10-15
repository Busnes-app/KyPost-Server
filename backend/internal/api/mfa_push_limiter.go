package api

import "time"

// mfaPushWindow and mfaPushBurst bound how often a login can fan an actual MFA
// push notification out to a given account's approver devices: without a cap, an
// attacker who already holds a valid password can mint an unlimited number of
// real MFA challenges by calling login repeatedly (each successful password
// check clears loginLockout), bombarding the user's devices with "Approve
// sign-in" pushes until one gets tapped out of habit or annoyance — the "MFA
// fatigue" pattern used in the 2022 Uber breach.
//
// This caps the number of pushes per window rather than allowing exactly one,
// which is what it used to do. A flat one-per-five-minutes was unshippable in
// practice: every login attempt mints a *fresh* challenge id and fresh
// MatchDigits (mfa.Store.Create), so the second attempt inside the window handed
// the browser a challenge that no device had ever been pushed. The browser then
// polled it until the TTL expired, the phone stayed dark, and push MFA looked
// broken after exactly one successful sign-in. Retrying is normal behaviour, not
// an attack.
//
// A burst of 3 is a deliberate posture, not a guess. Number matching
// (mfa.Challenge.MatchDigits) already means a prompt cannot be approved by a
// blind "yes" — the approver has to read digits off the browser that started the
// sign-in — so the fatigue risk a hard one-per-window was buying down is already
// carried by a stronger control. Three prompts is comfortably more than a real
// user needs and far short of the volume a fatigue attack relies on.
//
// This still does not block login or challenge creation itself (a user who
// mistyped a TOTP code must be able to retry), only how often the push fans out.
// When it does throttle, the login response stops advertising "push" as a usable
// method — see handleLogin. Advertising a method whose notification was
// suppressed is what produced the original silent hang.
const (
	mfaPushWindow = 5 * time.Minute
	mfaPushBurst  = 3
)

// newMfaPushLimiter keys on the account's user ID (not username+IP like
// loginLockout): the cap protects that account's devices wherever the login
// attempts come from. One push refills per window, so no window ever holds more
// than mfaPushBurst pushes; after the burst, it is one per window.
func newMfaPushLimiter() *keyedLimiter {
	return newKeyedLimiter(mfaPushBurst, 1/mfaPushWindow.Seconds())
}
