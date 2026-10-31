// Sign-in and session identity: password login, the second-factor challenge
// completions (TOTP, recovery code), session mint/teardown, and the middleware
// every authenticated route goes through (withAuth, withMailAuth, csrfCheckOK,
// currentUser). Keep the lockout key, the case-folding account lookup, and the
// credential check within reading distance: they have to agree on what "the
// same account" means, and that agreement is invisible from any one of them.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/totp"
	"github.com/Busnes-app/kypost-server/backend/internal/captcha"
	"github.com/Busnes-app/kypost-server/backend/internal/mfa"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Session tracks who a live session token belongs to.
//
// Sessions are in-memory only (Server.sessions) and never persisted: persisting
// them would write bearer-equivalent credentials to the same volume this project
// keeps free of plaintext secrets. A stolen token cannot outlive the process,
// and revocation is a map delete that cannot fail halfway. The cost is that any
// restart logs everyone out, and the process does not decide when it restarts.
// Only the API process has sessions; a second replica would not share them.
//
// Role is deliberately not stored here: currentUser looks the user up live on
// every request, so a role change or deactivation takes effect on the next
// request rather than at next login. CSRFToken backs the double-submit check
// (see csrfCheckOK), mirrored into the non-HttpOnly csrf_token cookie.
type Session struct {
	UserID string
	// IssuedAt is when this session was minted and never moves; ExpiresAt slides
	// forward on every request so an active user is not logged out mid-work.
	// sessionMaxLifetime past IssuedAt the session dies regardless of activity,
	// without which a stolen cookie is valid forever — the thief's own polling
	// keeps renewing it.
	IssuedAt  time.Time
	ExpiresAt time.Time
	CSRFToken string
	// SSOAppAdmin is the verified role ceiling of an SSO session: whether the
	// ID token that minted it carried the admin app role (or, for a generic
	// provider, an admin group). An SSO session never acts as admin without
	// it, whatever users.json says, so the provider's word bounds the local
	// role rather than replacing it. Password sessions have no ceiling.
	SSOAppAdmin bool
	// SSOKySignOn is true when the token that minted this SSO session spoke
	// the KySignOn contract. Only such a session can re-prove an action to
	// KySignOn; one from a generic provider keeps the password step-up, which
	// fails closed on its own for an account with no credential.
	SSOKySignOn bool
	// SSOEpoch is Server.ssoEpoch when the callback began, so a callback that
	// read settings before a trust change cannot mint under them.
	SSOEpoch uint64
	// SSOLinkGrantedAt is when this session last proved its credential and
	// second factor at handleSSOLinkStart. It authorizes exactly one SSO link
	// and is cleared when that link is written, so it cannot be replayed.
	//
	// Server-side deliberately. The obvious alternative — a signed ticket in the
	// state cookie — has to be unforgeable precisely BECAUSE the cookie is
	// attacker-writable: whoever holds the session controls their own cookie
	// jar. A fact recorded on the session cannot be written by the caller at
	// all, so there is nothing to forge and no token to leak.
	SSOLinkGrantedAt time.Time
	// SSOLinkState is the state token of the ONE flow SSOLinkGrantedAt
	// authorizes. Without it the grant authorizes any link-mode callback that
	// arrives in the window, and the state cookie is unsigned and
	// caller-written — so someone holding the session could start their own
	// plain sign-in flow, relabel its cookie link-mode, and spend a grant the
	// real user minted seconds earlier, binding their own subject. The state is
	// minted into the victim's cookie and never shown to anyone else, so
	// requiring it back is what ties the grant to the flow it paid for.
	SSOLinkState string
	// SSO names the provider login this session was minted from, so a
	// back-channel logout token can find it. Zero for a password login.
	SSO sso.SessionIdentity
}

const (
	// sessionIdleTimeout is how long a session survives with no requests.
	sessionIdleTimeout = 24 * time.Hour
	// sessionMaxLifetime is the absolute ceiling from IssuedAt, renewals
	// notwithstanding.
	sessionMaxLifetime = 7 * 24 * time.Hour
	// sessionSweepInterval is how often StartSessionSweeper reclaims
	// sessions that expired without anyone presenting them again.
	sessionSweepInterval = time.Hour
	// sessionSlideGranularity is how much the idle window must have advanced
	// before currentUser rewrites a session's ExpiresAt — i.e. the resolution
	// sessionIdleTimeout is actually enforced at. Noise against a 24-hour horizon,
	// and it is what keeps the common request path on a read lock.
	sessionSlideGranularity = 5 * time.Minute
)

// AuthContext identifies the caller of an authenticated request.
type AuthContext struct {
	// Private witnesses for durable native sends, never client-supplied.
	NativeSendEpoch    uint64
	DeviceWitness      string
	UserID             string
	Username           string
	Role               users.Role
	MustChangePassword bool
	// DeviceID names the paired device when the request authenticated with a
	// device credential (withMailAuth's device arm); empty for a session.
	DeviceID string
	// Mailbox is the extra mailbox an X-KyPost-Mailbox header selected and
	// withMailbox admitted; empty means the primary (mailbox ID == UserID).
	Mailbox string
	// SSOSession is true for a session minted by a KySignOn login, one whose
	// token spoke the KySignOn contract. Such a session re-authenticates to
	// KySignOn rather than with a password; a session from any other
	// provider is not marked and keeps the password gate.
	SSOSession bool

	// SessionCSRFToken is the CSRF token of the session this request authenticated
	// with, and is EMPTY for every other authentication path (paired-device
	// headers, CardDAV Basic).
	//
	// It is how csrfCheckOK knows whether the caller presented an ambient,
	// browser-attached credential, rather than inferring it from whether a cookie
	// happened to be on the request. The old inference answered "no session found
	// for this cookie" by allowing the request, survivable only because the session
	// cookie is SameSite=Lax and never reaches a cross-site POST.
	SessionCSRFToken string
}

// verifyLogin is the unauthenticated credential check shared by handleLogin and
// handleReviewPairing: instance KDF budget, a read-only per-IP lock check,
// CAPTCHA, the per-IP and per-account lockouts, restore hold, then the auth
// secret, with failures timing-equalized. accountLockout is the caller's
// per-account table (loginLockout, or review pairing's own). admit, when set,
// is an extra account condition that fails like an unknown user. On false the
// response has been written.
func (s *Server) verifyLogin(w http.ResponseWriter, r *http.Request, accountLockout *failureLockout, username, authSecret, captchaToken string, admit func(users.User) bool) (users.User, bool) {
	// Instance-wide rate limit FIRST, before anything that costs real work.
	//
	// The per-account lockout below cannot bound total work: it is keyed on
	// username+IP and the username comes out of the request body, so a caller who
	// never repeats one never trips it — while every attempt against an unknown
	// account runs a full Argon2id derivation on purpose (see
	// equalizeLoginTiming).
	//
	// The bucket holds seconds of derivation work: this reserves one attempt's worth
	// and chargeLoginKDF settles up with what the attempt really cost, so a slower
	// machine drains the budget faster instead of waving through the same number of
	// requests to burn more CPU each. The reservation is released exactly once — by
	// chargeLoginKDF, or by the deferred refund on any path that derived nothing.
	// See loginBudget.
	var budget loginBudget
	if s.loginRateLimiter != nil {
		if ok, retryAfter := s.loginRateLimiter.admitCost(loginRateLimitKey, loginKDFReserveSeconds); !ok {
			retrySeconds := int(retryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":             "too many sign-in attempts right now, try again shortly",
				"retryAfterSeconds": retrySeconds,
			})
			return users.User{}, false
		}
		budget.limiter = s.loginRateLimiter
	}
	defer budget.refund()

	// Per-IP lockout, independent of the username. Closes the rotating-username
	// hole in the per-account budget below: 50 failures from one address in 15
	// minutes stops that address, whatever names it tried. Loose on purpose — a NAT
	// egress or an office must not be easier to lock out than an account. Login is
	// where a constant clientIP does the most damage, so it is where the proxy
	// misconfiguration announces itself.
	s.warnOnUnusedProxyHeaders(r)
	ipLockoutKey := lockoutKeyForIP(clientIP(r))
	ipLocked := func(retryAfter time.Duration) {
		retrySeconds := int(retryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":             "too many failed attempts from this address, try again later",
			"retryAfterSeconds": retrySeconds,
		})
	}
	// Read-only, so a locked address never reaches the CAPTCHA provider.
	if wait := s.loginIPLockout.lockedFor(ipLockoutKey); wait > 0 {
		ipLocked(wait)
		return users.User{}, false
	}

	// CAPTCHA before any lockout write: a failed solution is refunded from the
	// budget above, so if it created per-account entries, junk tokens could fill
	// that table for free.
	if s.captchaVerifier != nil {
		ok, err := s.captchaVerifier.Verify(r.Context(), captchaToken, clientIP(r))
		switch {
		case errors.Is(err, captcha.ErrChallengeExpired):
			// Self-hosted proof-of-work only: a signed solution past its deadline, a
			// tab left open. 401, not the provider-down 503 below.
			http.Error(w, "security check expired, please try again", http.StatusUnauthorized)
			return users.User{}, false
		case errors.Is(err, captcha.ErrChallengeWrongClient):
			// Self-hosted proof-of-work only: a valid solution from a different address
			// than it was issued to, e.g. a phone moving from wifi to cellular.
			http.Error(w, "your network address changed during the security check, please try again", http.StatusUnauthorized)
			return users.User{}, false
		case err != nil:
			s.logger.Error("captcha verification failed", "error", err.Error())
			http.Error(w, "captcha verification unavailable", http.StatusServiceUnavailable)
			return users.User{}, false
		case !ok:
			// A strike on the address only, so one address cannot keep provider
			// calls in flight (each holds instance budget) indefinitely.
			s.loginIPLockout.tryAttempt(ipLockoutKey)
			http.Error(w, "captcha verification failed", http.StatusUnauthorized)
			return users.User{}, false
		}
	}

	allowed, retryAfter, ipStrike := s.loginIPLockout.tryAttempt(ipLockoutKey)
	if !allowed {
		ipLocked(retryAfter)
		return users.User{}, false
	}

	// Three-strikes/15-minute lockout, keyed on username+client IP: on the username
	// whether or not it exists (so lockout behavior cannot enumerate accounts), and
	// on the IP (so hammering a known username cannot lock its real owner out from
	// their own machine).
	//
	// The username MUST be folded through users.NormalizeUsername — the same fold
	// GetByUsername resolves the account with. On the raw string, "victim",
	// "Victim" and " victim " are one account to the lookup but three strike
	// budgets here, and padding makes that key space unbounded.
	// Clamped: NormalizeUsername folds case and trims but does not truncate, and
	// the body allows 64 KiB, so without this the caller sizes the lockout
	// table's memory as well as populating it.
	lockoutKey := clampLockoutKeyComponent(users.NormalizeUsername(username)) + "\x00" + lockoutKeyForIP(clientIP(r))
	allowed, retryAfter, strike := accountLockout.tryAttempt(lockoutKey)
	if !allowed {
		retrySeconds := int(retryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":             "too many failed attempts, try again later",
			"retryAfterSeconds": retrySeconds,
		})
		return users.User{}, false
	}

	// The account this attempt targeted, folded the same way GetByUsername
	// resolves it and the lockout key above is built — so escalation, lockout
	// and lookup all agree on what "the same account" means.
	powAccount := clampLockoutKeyComponent(users.NormalizeUsername(username))

	// Failures are padded from here (settleCredentialTiming); before the lookup,
	// whose own cost differs between a hit and a miss.
	credentialStart := time.Now()
	u, err := s.users.GetByUsername(username)
	if err == nil && u.NeedsPasswordReset() {
		s.logger.Warn("sign-in refused: legacy password hash, admin password reset required", "user_id", u.ID)
	}
	// A legacy account fails exactly like an unknown one, so this public
	// endpoint is no oracle for which accounts await a reset.
	if err != nil || !u.Active || !u.UsesDerivedAuth() || s.requireAccountRestoreReleased(u) != nil || admit != nil && !admit(u) {
		// Do the same derivation work a real check would, so neither timing nor
		// slot contention reveals whether the username exists (or is inactive).
		if _, err := s.chargeLoginKDF(r.Context(), &budget, func(ctx context.Context) bool {
			// The error is discarded here because the enclosing chargeLoginKDF
			// already holds the slot, so this derivation cannot be shed — see
			// users.WithKDFSlot.
			_ = s.equalizeLoginTiming(ctx, authSecret)
			return false
		}); err != nil {
			// Shed before the equalization ran. The refund is not just
			// courtesy: answering 401 instantly on the unknown-username path
			// while a real account still waits for a slot is the timing
			// disclosure equalizeLoginTiming exists to erase.
			accountLockout.cancelAttempt(strike)
			s.loginIPLockout.cancelAttempt(ipStrike)
			writeKDFBusy(w)
			return users.User{}, false
		}
		s.settleCredentialTiming(r.Context(), credentialStart)
		// Make the next challenge from this address more expensive. Recorded
		// for the unknown-username case too: spraying a list of guessed
		// usernames is exactly the pattern this is here to price.
		s.powDifficulty.recordFailure(lockoutKeyForIP(clientIP(r)), powAccount, time.Now())
		// No recordFailure on the lockout: tryAttempt already spent the strike.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return users.User{}, false
	}
	// Runs inside chargeLoginKDF's slot, so the busy error is unreachable here
	// and a false really is a wrong credential.
	verified, err := s.chargeLoginKDF(r.Context(), &budget, func(ctx context.Context) bool {
		ok, _ := users.VerifyAuthSecret(ctx, u, authSecret)
		return ok
	})
	if err != nil {
		// Saturated slots, not a wrong password. Refund both strikes: a 401 here would
		// spend a third of this account's budget on a credential nobody looked at, so
		// three unlucky arrivals during a burst would lock out a user who typed
		// everything correctly. recordFailure is skipped for the same reason.
		accountLockout.cancelAttempt(strike)
		s.loginIPLockout.cancelAttempt(ipStrike)
		writeKDFBusy(w)
		return users.User{}, false
	}
	if !verified {
		s.settleCredentialTiming(r.Context(), credentialStart)
		s.powDifficulty.recordFailure(lockoutKeyForIP(clientIP(r)), powAccount, time.Now())
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return users.User{}, false
	}
	accountLockout.recordSuccess(lockoutKey)
	// Refund only this attempt's strike: clearing the address's budget let one
	// valid account reset failures spent guessing other accounts' passwords.
	s.loginIPLockout.cancelAttempt(ipStrike)

	// Transparently upgrade a hash written at an older, cheaper Argon2id cost.
	// Best-effort: a failure keeps the old hash and retries next sign-in.
	if users.NeedsRehash(u.PasswordHash) {
		if err := s.users.RehashPassword(r.Context(), u.ID, authSecret); err != nil {
			s.logger.Error("password hash upgrade failed", "user_id", u.ID, "error", err.Error())
		}
	}
	// A correct password proves whoever is at this address holds THIS
	// account's credential — and nothing about any other account they have
	// been guessing at from the same address. Forgive only this one; see
	// powEscalation.clearAccount.
	s.powDifficulty.clearAccount(lockoutKeyForIP(clientIP(r)), powAccount)
	return u, true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		// AuthSecret is the client-derived authentication half of the stretched
		// password — see login_params.go and frontend/src/lib/authSecret.ts. The
		// key-wrapping half never leaves the browser. There is no plaintext
		// password field: a body carrying one is ignored and fails as a wrong
		// credential.
		AuthSecret   string `json:"authSecret"`
		CaptchaToken string `json:"captchaToken,omitempty"`
	}
	// Bounded before it is buffered. This is the only unauthenticated decode in the
	// codebase and it runs before the lockout and captcha checks below, so an
	// unbounded body let any anonymous caller choose the server's allocation:
	// json.Decode buffers the whole value and then allocates the string on top,
	// measured at ~5.6x the wire size.
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	u, ok := s.verifyLogin(w, r, s.loginLockout, req.Username, req.AuthSecret, req.CaptchaToken, nil)
	if !ok {
		return
	}

	// Second-factor users must clear a challenge before a session exists. No
	// cookie is set here; the client receives a challenge id plus the methods it
	// may use. A push-enabled challenge additionally fans a notification out to
	// the user's approver devices (asynchronously — see dispatchPushChallenge).
	if u.TOTPEnabled || u.PushMFAEnabled {
		if u.PushMFAEnabled {
			store, err := s.userStore(u.ID)
			if err != nil {
				http.Error(w, "failed to open user state", http.StatusServiceUnavailable)
				return
			}
			if _, err := mfaApproverDevices(store); err != nil {
				http.Error(w, "failed to read paired devices", http.StatusServiceUnavailable)
				return
			}
		}
		ch, err := s.mfaChallenges.Create(u.ID)
		if err != nil {
			http.Error(w, "session creation failed", http.StatusInternalServerError)
			return
		}
		methods := make([]string, 0, 2)
		if u.TOTPEnabled {
			methods = append(methods, "totp")
		}
		// Rate-limit the push itself, not challenge creation or login: a user who
		// mistyped a TOTP code must still be able to retry. See mfaPushLimiter for
		// why this is a burst cap rather than one-per-window.
		pushDispatched, pushRetryAfter := false, time.Duration(0)
		if u.PushMFAEnabled {
			if allowed, retryAfter := s.mfaPushLimiter.allow(u.ID); allowed {
				// At most one challenge for this account is ever both pushed and answerable.
				// Earlier attempts that were never answered are dropped rather than left
				// polling an id no device was told about — which, with the old
				// one-push-per-window cap, is how push MFA came to look broken after the first
				// sign-in.
				s.mfaChallenges.SupersedeUnansweredPush(u.ID, ch.ID)
				pushDispatched = true
				// Snapshot the request context before the goroutine: r is not
				// safe to touch once this handler returns.
				go s.dispatchPushChallenge(u.ID, ch.ID, newLoginContext(r), ch.CreatedAt, ch.MatchDigits, ch.DecoyDigits)
			} else {
				pushRetryAfter = retryAfter
			}
		}
		// Offered only when a notification actually went out for THIS challenge.
		// Advertising "push" for a challenge whose push was suppressed left the
		// browser polling a challenge no device could answer, with nothing on the
		// phone and no explanation, until the TTL ran out.
		if pushDispatched {
			methods = append(methods, "push")
		}
		resp := map[string]any{
			"mfaRequired": true,
			"challengeId": ch.ID,
			"methods":     methods,
		}
		if pushDispatched {
			// The number the approving device must send back. Safe to hand to
			// this caller: they are the one being asked to read it off this
			// screen, and knowing it proves nothing on its own — approving
			// still needs a paired device's credentials.
			resp["matchDigits"] = ch.MatchDigits
			// The credential /api/auth/mfa/push/finish requires. This response
			// is the only place it appears; it is deliberately kept out of the
			// notification payload, which every hop of the relay path reads.
			resp["finishSecret"] = ch.FinishSecret
		}
		if pushRetryAfter > 0 {
			// So the UI can say "you have requested too many approvals, try again
			// in N seconds" instead of silently falling back to TOTP with no
			// account of where the push option went.
			resp["pushRetryAfterSeconds"] = int(pushRetryAfter.Seconds()) + 1
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if err := s.startSession(w, r, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

// handleCaptchaConfig is public (pre-login) and tells the frontend which
// CAPTCHA widget, if any, to render on the login form. provider=="" means
// CAPTCHA is disabled. siteKey is the provider's public site key — safe to
// expose, unlike the secret key used server-side for verification.
func (s *Server) handleCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": s.captchaProvider,
		"siteKey":  s.captchaSiteKey,
	})
}

// handleCSRFToken returns the CSRF token paired with the caller's session, for
// same-origin JS that cannot read the non-HttpOnly csrf_token cookie —
// specifically the service worker's pushsubscriptionchange handler, which must
// send X-CSRF-Token but has no access to document.cookie. The response carries
// no CORS headers, so a cross-origin page can trigger this GET but never read
// the token; possession of the session cookie remains the only way to obtain it.
func (s *Server) handleCSRFToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	cookie, err := r.Cookie("kypost_session")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	// Read lock: this is a lookup, and the whole reason cfgMu and sessMu were
	// split off the old single mu is that taking sessMu exclusively on a request
	// path serializes every other request behind it. csrfCheckOK does the
	// identical lookup under RLock thirty lines below.
	s.sessMu.RLock()
	sess, ok := s.sessions[cookie.Value]
	s.sessMu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"csrfToken": sess.CSRFToken})
}

// startSession mints a session token for userID, records it, and sets the
// kypost_session cookie with exactly the flags the legacy password-only login
// used. Shared by handleLogin and the second-factor endpoints.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	return s.mintSession(w, r, Session{UserID: userID})
}

// mintSession is startSession for a caller that has more to record than the
// user id: the SSO callback stores the provider login the session came from.
func (s *Server) mintSession(w http.ResponseWriter, r *http.Request, sess Session) error {
	u, err := s.users.Get(sess.UserID)
	if err != nil {
		return err
	}
	if !u.Active {
		return errors.New("account is inactive")
	}
	if err := s.requireAccountRestoreReleased(u); err != nil {
		return err
	}
	token, err := randomToken(24)
	if err != nil {
		return err
	}
	csrfToken, err := randomToken(24)
	if err != nil {
		return err
	}
	now := time.Now()
	sess.IssuedAt, sess.ExpiresAt, sess.CSRFToken = now, now.Add(sessionIdleTimeout), csrfToken
	s.sessMu.Lock()
	// Under sessMu, which endSSOSessions holds while bumping the epoch.
	if sess.SSO.Subject != "" && sess.SSOEpoch != s.ssoEpoch.Load() {
		s.sessMu.Unlock()
		return errors.New("SSO settings changed during sign-in")
	}
	s.sessions[token] = sess
	s.sessMu.Unlock()
	secure := isRequestSecure(r)
	http.SetCookie(w, &http.Cookie{Name: "kypost_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	// Deliberately NOT HttpOnly: the frontend must be able to read this and
	// echo it back as the X-CSRF-Token header (double-submit pattern) — see
	// csrfCheckOK. It carries no authority on its own without the paired
	// HttpOnly session cookie, so JS-readability doesn't weaken the session.
	http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: csrfToken, Path: "/", HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode})
	return nil
}

// handleMFATOTP completes a login challenge with a TOTP code. It is
// authenticated solely by possession of a valid challengeId (no session
// cookie). On success it mints the real session.
func (s *Server) handleMFATOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ch, ok := s.mfaChallenges.Get(strings.TrimSpace(req.ChallengeID))
	if !ok {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	// Per-account throttle spanning challenges: the per-challenge attempt cap
	// alone can be reset by minting a new challenge, so a password-holding
	// attacker could otherwise brute force TOTP online.
	allowed, _, strike := s.mfaLockout.tryAttempt(ch.UserID)
	if !allowed {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}

	u, err := s.users.Get(ch.UserID)
	if err != nil || !u.Active || s.requireAccountRestoreReleased(u) != nil || !u.TOTPEnabled || u.TOTPSecretEnc == "" {
		// The account changed underneath the challenge; no code was offered,
		// so the strike tryAttempt reserved goes back.
		s.mfaLockout.cancelAttempt(strike)
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	secret, err := mfa.OpenTOTPSecret(u.TOTPSecretEnc, s.totpSecretKeyPath)
	if err != nil {
		s.mfaLockout.cancelAttempt(strike)
		http.Error(w, "failed to load second factor", http.StatusInternalServerError)
		return
	}

	step, valid := totp.Validate(secret, req.Code, time.Now())
	if !valid {
		// tryAttempt already spent the strike.
		if err := s.mfaChallenges.RecordTOTPAttempt(ch.ID); errors.Is(err, mfa.ErrTooManyAttempts) {
			http.Error(w, "too many attempts", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}

	// A challenge is single-use: ConsumeTOTPStep atomically checks-and-marks
	// consumption under a single lock, so two concurrent requests bearing the
	// same still-valid code cannot both win (closes the TOCTOU window a
	// separate Get + later RecordTOTPStep would leave open).
	if err := s.mfaChallenges.ConsumeTOTPStep(ch.ID, step); err != nil {
		if errors.Is(err, mfa.ErrChallengeAlreadyUsed) {
			http.Error(w, "challenge already used", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	// Per-account replay guard. Single-use protection on the challenge alone is not
	// enough: a password-holding attacker can mint unlimited challenges, so a
	// captured valid code would be replayable once per fresh one.
	// SetLastUsedTOTPStep rejects any step not strictly newer than the last accepted
	// for this account, across challenges.
	//
	// Ordering is load-bearing: this runs after every other check so a rejected code
	// never advances the recorded step and answers exactly like a wrong code, and
	// recordSuccess stays below it so a replay counts against the lockout instead of
	// clearing it.
	if _, err := s.users.SetLastUsedTOTPStep(u.ID, step); err != nil {
		s.mfaChallenges.Delete(ch.ID)
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	s.mfaLockout.recordSuccess(ch.UserID)

	s.mfaChallenges.Delete(ch.ID)
	if err := s.startSession(w, r, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

// handleMFARecoveryCode completes a login challenge with a one-time recovery
// code. The matched code is consumed (removed) on success.
func (s *Server) handleMFARecoveryCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ch, ok := s.mfaChallenges.Get(strings.TrimSpace(req.ChallengeID))
	if !ok {
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}
	allowed, _, strike := s.mfaLockout.tryAttempt(ch.UserID)
	if !allowed {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}
	u, err := s.users.Get(ch.UserID)
	if err != nil || !u.Active || s.requireAccountRestoreReleased(u) != nil || !u.TOTPEnabled {
		s.mfaLockout.cancelAttempt(strike)
		http.Error(w, "invalid or expired challenge", http.StatusUnauthorized)
		return
	}

	var matched bool
	_, matched, err = s.users.ConsumeRecoveryCode(r.Context(), u.ID, strings.TrimSpace(req.Code), s.recoveryCodeDigest)
	if err != nil {
		s.mfaLockout.cancelAttempt(strike)
		http.Error(w, "failed to verify recovery code", http.StatusInternalServerError)
		return
	}
	if !matched {
		if err := s.mfaChallenges.RecordTOTPAttempt(ch.ID); errors.Is(err, mfa.ErrTooManyAttempts) {
			http.Error(w, "too many attempts", http.StatusUnauthorized)
			return
		}
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	s.mfaLockout.recordSuccess(ch.UserID)

	s.mfaChallenges.Delete(ch.ID)
	if err := s.startSession(w, r, u.ID); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mustChangePassword": u.MustChangePassword})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("kypost_session")
	if err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	secure := isRequestSecure(r)
	http.SetCookie(w, &http.Cookie{Name: "kypost_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// currentSessionToken returns the kypost_session cookie value on r, or "" if
// absent — used alongside revokeUserSessions so a self-service credential
// change revokes every *other* session without also logging out the request
// that made the change.
func currentSessionToken(r *http.Request) string {
	if c, err := r.Cookie("kypost_session"); err == nil {
		return c.Value
	}
	return ""
}

// revokeUserSessions deletes every live session belonging to userID, except
// keepToken if non-empty (the caller's own session, when the caller and the
// affected account are the same). Called after a password change/reset or MFA
// disable/regeneration so a stolen session cookie is cut off as soon as the
// legitimate user, or an admin on their behalf, takes one of those recovery
// actions, rather than staying valid for the remaining 24h sliding window.
func (s *Server) revokeUserSessions(userID, keepToken string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for token, sess := range s.sessions {
		if sess.UserID == userID && token != keepToken {
			delete(s.sessions, token)
		}
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := s.currentUser(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	// Read, never mint: this is a GET the frontend polls on every auth refresh,
	// and GetOrCreateSubscriberID made each one a file-locked write. The mint
	// belongs to handleNotificationPairing, which is where a subscriber ID is
	// actually about to be used.
	subscriberID := ""
	if store, err := s.userStore(ac.UserID); err == nil {
		subscriberID = store.SubscriberID()
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "subscriberId": subscriberID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":      true,
		"userId":             u.ID,
		"username":           u.Username,
		"role":               ac.Role,
		"mustChangePassword": u.MustChangePassword,
		"hasPassword":        u.HasLocalCredential(),
		"ssoSession":         ac.SSOSession,
		"subscriberId":       subscriberID,
		"ssoSub":             u.SSOSub,
		"ssoUsername":        u.SSOUsername,
		"ssoEmail":           u.SSOEmail,
		"ssoLinkedAt":        u.SSOLinkedAt,
		"ssoLinkRevoked":     u.SSOLinkRevoked(),
	})
}

// handlePasswordSnapshot exposes only preparation data for the password change.
// Forced-reset credentials cannot open the previous PGP envelope: preserve it
// for recovery, and do not widen the reset session's access to key material.
func (s *Server) handlePasswordSnapshot(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	wrapped := u.PGPPrivateKeyWrapped
	if u.MustChangePassword {
		wrapped = ""
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"pgpRevision":        u.PGPRevision,
		"keyring":            u.PGPKeyring,
		"protection":         u.PGPProtection(),
		"wrappedPrivateKey":  wrapped,
		"mustChangePassword": u.MustChangePassword,
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		// NewAuthSecret plus NewLoginSalt/NewIterations replace the credential
		// without the server ever seeing the new password; OldAuthSecret proves
		// the current one. No plaintext fields exist.
		OldAuthSecret string `json:"oldAuthSecret,omitempty"`
		NewAuthSecret string `json:"newAuthSecret,omitempty"`
		NewLoginSalt  string `json:"newLoginSalt,omitempty"`
		NewIterations int    `json:"newIterations,omitempty"`

		// RewrappedPGPKey is the client-wrapped private-key envelope re-sealed under
		// the NEW password, written in the SAME transaction as the credential.
		//
		// As a separate follow-up request, a dropped connection in between left the
		// password changed and the envelope still sealed under the old one —
		// permanently, because the only rewrap path re-derives from the CURRENT
		// password and could never open it again.
		RewrappedPGPKey  string  `json:"rewrappedPgpKey,omitempty"`
		KeyringVersion   *int    `json:"keyringVersion,omitempty"`
		ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}

	// Verify the CURRENT credential.
	//
	// Behind a lockout, and every derivation on this path is admitted by the
	// shared slots. This is three 128 MiB derivations per request on an endpoint
	// any authenticated account can call in a loop: the check here, plus the hash
	// and any rewrap below. The comment used to say all three were "under a KDF
	// slot" while only this one was — SetPassword and SetDerivedAuthAndRewrapPGP
	// reached scrypt on their own, which is two thirds of the cost of the most
	// expensive authenticated endpoint sitting outside the ceiling. They now take
	// the request context and are admitted like everything else.
	// This request always verifies the current credential. There is no
	// MustChangePassword exemption, and there was one.
	//
	// The exemption's stated reasoning — "a user handed a temporary password may
	// have nothing to prove" — is false as implemented. Every path to a session
	// on such an account runs through handleLogin, or through an MFA completion
	// whose challenge is minted only after a successful password login, so the
	// caller demonstrably HAS the temporary password. The shipped SPA proves it
	// too: LoginPage refuses to submit this form without a current password and
	// always sends credentialFields(oldCredential, "old").
	//
	// What the exemption actually bought was a password reset for anyone holding
	// a cookie. A request carrying only {newAuthSecret, newLoginSalt,
	// newIterations} verified nothing, returned 200, installed the attacker's
	// secret, killed the temporary password, cleared MustChangePassword, and
	// then revokeAllUserCredentialsExcept evicted the owner while preserving the
	// attacker — against every admin-created and admin-reset account, for the
	// whole forced-change window. csrf_token is deliberately non-HttpOnly, so
	// XSS reaches this. 2f0e9d9 gated the recoverable half (the PGP rewrap, an
	// irreversible write) and left the credential half open.
	//
	// The existing test's own comment named the risk: "Inverted, this branch
	// turns the endpoint into a password reset for anyone holding a cookie."
	//
	// Keyed on user AND client IP, like the login lockout and for the same reason:
	// on the user alone, an attacker holding a stolen cookie burns the whole budget
	// from their own machine and locks the real owner out of changing their
	// password — during the incident where changing it is the remedy. Keyed on the
	// pair, a thief locks out only themselves.
	lockKey := ac.UserID + "\x00" + lockoutKeyForIP(clientIP(r))
	allowed, retryAfter, strike := s.passwordChangeLockout.tryAttempt(lockKey)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":             "too many failed attempts, try again later",
			"retryAfterSeconds": int(retryAfter.Seconds()) + 1,
		})
		return
	}
	okCurrent, err := users.VerifyAuthSecret(r.Context(), u, req.OldAuthSecret)
	if err != nil {
		// Shed before the derivation ran. Refund: no credential was
		// examined, so this attempt must not count against the budget.
		s.passwordChangeLockout.cancelAttempt(strike)
		writeKDFBusy(w)
		return
	}
	if !okCurrent {
		// tryAttempt already spent the strike.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	s.passwordChangeLockout.recordSuccess(lockKey)

	if req.KeyringVersion != nil {
		if req.ExpectedRevision == nil || *req.ExpectedRevision == 0 {
			writeUserStoreError(w, users.ErrInvalidPGPRevision)
			return
		}
		if *req.ExpectedRevision != u.PGPRevision {
			writeUserStoreError(w, users.ErrPGPRevisionChanged)
			return
		}
	}
	if req.KeyringVersion != nil && (*req.KeyringVersion != 1 || req.NewAuthSecret == "" || req.RewrappedPGPKey == "") {
		http.Error(w, "keyringVersion 1 requires a complete rewrapped keyring and derived credential", http.StatusBadRequest)
		return
	}

	if req.NewAuthSecret == "" || req.NewLoginSalt == "" {
		http.Error(w, "newAuthSecret and newLoginSalt are required", http.StatusBadRequest)
		return
	}
	iterations := req.NewIterations
	if iterations <= 0 {
		iterations = users.LoginIterations()
	}
	// One mutation for the credential and the PGP envelope, or neither.
	var updated users.User
	if req.KeyringVersion != nil {
		updated, err = s.users.ChangeKeyringPassword(r.Context(), u.ID, users.PGPKeyringCredential{
			AuthSecret: req.NewAuthSecret, LoginSalt: req.NewLoginSalt, Iterations: iterations,
		}, req.RewrappedPGPKey, req.ExpectedRevision)
	} else {
		updated, err = s.users.SetDerivedAuthAndRewrapPGP(
			r.Context(), u.ID, req.NewAuthSecret, req.NewLoginSalt, iterations, false, req.RewrappedPGPKey, req.ExpectedRevision,
		)
	}
	if err != nil {
		if errors.Is(err, users.ErrPGPRevisionChanged) || errors.Is(err, users.ErrInvalidPGPRevision) || errors.Is(err, users.ErrPGPKeyringUpgradeRequired) {
			writeUserStoreError(w, err)
			return
		}
		if errors.Is(err, users.ErrKDFBusy) {
			writeKDFBusy(w)
			return
		}
		if errors.Is(err, users.ErrAuthSecretMalformed) || errors.Is(err, users.ErrNotClientProtected) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "failed to update password", http.StatusInternalServerError)
		return
	}
	// A password change is what a user reaches for when they think they have been
	// compromised, so it must cut off EVERY credential, not just sessions: a device
	// secret minted from a stolen session is independent of the password and would
	// otherwise keep full mailbox access and, if flagged as an approver, a
	// standing second factor. The caller's own session is preserved.
	if err := s.revokeAllUserCredentialsExcept(u, currentSessionToken(r)); err != nil {
		http.Error(w, "password changed but credential revocation failed; retry immediately", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": updated.PGPRevision})
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := s.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if !csrfCheckOK(r, ac) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "missing or invalid csrf token"})
			return
		}
		// Enforce the first-login password change server-side: a user who still owes
		// one (e.g. the bootstrap admin) gets a full session but may reach nothing
		// except the change/logout endpoints. Without this the flag is merely advisory
		// and a default credential grants full access.
		if ac.MustChangePassword && !mustChangePasswordExemptPaths[r.URL.Path] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "password change required", "mustChangePassword": true})
			return
		}
		// Per-account meter on MUTATING requests. A session is not a licence to
		// drive whole-file users.json rewrites in a loop — every account
		// mutation marshals and fsyncs the entire file under a global
		// cross-process lock that every authenticated request also reads
		// through, so one looping session stalls the instance. Reads are
		// untouched. See accountWriteBurst.
		if !s.meterAccountWrite(w, r, ac.UserID) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, ac)))
	}
}

// csrfCheckOK enforces a double-submit CSRF check on cookie-authenticated,
// state-changing (non-GET/HEAD/OPTIONS) requests: the X-CSRF-Token header must
// match the csrf_token minted alongside the caller's session (see startSession).
//
// It takes the RESOLVED AuthContext rather than re-deriving one from the
// request. Looking the cookie up here means answering "I don't know" — no
// cookie, or a cookie matching no session — with "allow", which leaves
// SameSite=Lax as the only real control. TestSessionCookieStaysSameSiteLax pins
// that attribute; this function no longer depends on it.
//
// Callers that did not authenticate by cookie — paired mobile clients
// (X-Kypost-Device-Id/X-Kypost-Device-Secret, see resolveMailAuthContext) and
// CardDAV (HTTP Basic) — leave SessionCSRFToken empty and are structurally
// exempt: their credential is not attached by the browser.
func csrfCheckOK(r *http.Request, ac AuthContext) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if ac.SessionCSRFToken == "" {
		return true
	}
	header := r.Header.Get("X-CSRF-Token")
	return header != "" && subtle.ConstantTimeCompare([]byte(header), []byte(ac.SessionCSRFToken)) == 1
}

// withMailAuth gates endpoints mobile clients need to reach without a web
// session — mail read/act-on (inbox, folders, actions, draft, send), contacts
// dedupe/groups/photo-get, and the PGP QR token mint — for either a web session
// cookie or a paired device's own X-Kypost-Device-Id/X-Kypost-Device-Secret
// credentials (see resolveMailAuthContext). Despite the name it is no longer
// mail-exclusive; IMAP/SMTP account setup (/api/imap/config, /api/imap/test)
// and other web-UI-only writes intentionally stay on withAuth only.
func (s *Server) withMailAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, err := s.resolveMailAuthContext(r)
		if err != nil {
			if s.refuseNativeAdministrator(w, r, ac.UserID, err) {
				return
			}
			var lockErr *mailLockedOutError
			if errors.As(err, &lockErr) {
				if lockErr.kdfBusy {
					writeKDFBusy(w)
					return
				}
				w.Header().Set("Retry-After", strconv.Itoa(int(lockErr.retryAfter.Seconds())+1))
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed attempts, try again later"})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if !csrfCheckOK(r, ac) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "missing or invalid csrf token"})
			return
		}
		// Session users still owing a first-login password change are blocked
		// here too (device-auth contexts never set this flag).
		if ac.MustChangePassword && !mustChangePasswordExemptPaths[r.URL.Path] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "password change required", "mustChangePassword": true})
			return
		}
		if !s.meterAccountWrite(w, r, ac.UserID) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, ac)))
	}
}

// meterAccountWrite applies the per-account write meter to a mutating request,
// writing the 429 and returning false when the caller is over budget.
//
// Extracted so every authentication wrapper can apply it. The meter was
// deliberately placed at the wrapper rather than at the endpoints so new routes
// would inherit the bound automatically — but only withAuth ever called it, so
// the ~26 withMailAuth routes, every device-credential route and every CardDAV
// route ran unmetered. Those wrappers carry rules/run, recipient resolution and
// pickup creation, which is what left the expensive paths with no rate bound at
// all.
//
// Reads are untouched, here as in withAuth: metering a mailbox listing would
// throttle ordinary use.
func (s *Server) meterAccountWrite(w http.ResponseWriter, r *http.Request, userID string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	ok, retryAfter := s.accountWriteLimiter.allow(userID)
	if ok {
		return true
	}
	seconds := int(retryAfter.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":             "too many requests, slow down",
		"retryAfterSeconds": seconds,
	})
	return false
}

type authContextKey struct{}

// authFromContext retrieves the AuthContext injected by withAuth or
// withDAVBasicAuth. It only returns ok=false if called on a request that
// never passed through either (a programming error), since both already
// reject the request before next() runs otherwise.
func authFromContext(r *http.Request) (AuthContext, bool) {
	return authContextFromContext(r.Context())
}

func authContextFromContext(ctx context.Context) (AuthContext, bool) {
	ac, ok := ctx.Value(authContextKey{}).(AuthContext)
	return ac, ok
}

// currentUser validates the session cookie and looks the owning user up
// live from the users store (not snapshotted into the session), so a role
// change or deactivation take effect on the request immediately following
// it rather than only at next login.
func (s *Server) currentUser(r *http.Request) (AuthContext, bool) {
	cookie, err := r.Cookie("kypost_session")
	if err != nil {
		return AuthContext{}, false
	}

	now := time.Now()
	// Read under the read lock first. The common case is a live session that was
	// already touched moments ago and does not need its expiry rewritten, and
	// taking the write lock for that turned every authenticated request into an
	// exclusive critical section — see the sessMu note on Server.
	s.sessMu.RLock()
	sess, ok := s.sessions[cookie.Value]
	s.sessMu.RUnlock()
	if !ok {
		return AuthContext{}, false
	}

	// Idle timeout, then the absolute cap. The cap is checked separately so
	// that renewing below can never push a session past sessionMaxLifetime.
	if now.After(sess.ExpiresAt) || now.Sub(sess.IssuedAt) >= sessionMaxLifetime {
		s.sessMu.Lock()
		// Re-check identity before deleting: this token could have been logged
		// out and a new session minted under the same map key in between (not
		// possible today — tokens are 24 random bytes — but deleting a session
		// this goroutine never validated is wrong regardless).
		if cur, still := s.sessions[cookie.Value]; still && cur.IssuedAt.Equal(sess.IssuedAt) {
			delete(s.sessions, cookie.Value)
		}
		s.sessMu.Unlock()
		return AuthContext{}, false
	}

	// Sliding idle window for active users; IssuedAt is deliberately carried
	// through unchanged so the absolute ceiling still applies. Only written when
	// the window has moved by sessionSlideGranularity — rewriting a 24-hour horizon
	// because 40 milliseconds elapsed buys nothing and costs an exclusive lock on
	// the hot path.
	if now.Sub(sess.ExpiresAt.Add(-sessionIdleTimeout)) >= sessionSlideGranularity {
		s.sessMu.Lock()
		if cur, still := s.sessions[cookie.Value]; still {
			cur.ExpiresAt = now.Add(sessionIdleTimeout)
			s.sessions[cookie.Value] = cur
		}
		s.sessMu.Unlock()
	}

	u, err := s.users.Get(sess.UserID)
	// An SSO session lives only as long as the link and the trust settings it
	// was minted under.
	ssoStale := sess.SSO.Subject != "" && (u.SSOLinkRevoked() || u.SSOSub != sess.SSO.Subject || sess.SSOEpoch != s.ssoEpoch.Load())
	if err != nil || !u.Active || ssoStale || s.requireAccountRestoreReleased(u) != nil {
		s.sessMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessMu.Unlock()
		return AuthContext{}, false
	}
	role := u.Role
	if sess.SSO.Subject != "" && !sess.SSOAppAdmin && role == users.RoleAdmin {
		role = users.RoleUser
	}
	return AuthContext{
		NativeSendEpoch:    u.NativeSendEpoch,
		UserID:             u.ID,
		Username:           u.Username,
		Role:               role,
		MustChangePassword: u.MustChangePassword,
		SSOSession:         sess.SSOKySignOn,
		// This request authenticated by cookie, so it carries an ambient
		// credential and csrfCheckOK must enforce the double submit.
		SessionCSRFToken: sess.CSRFToken,
	}, true
}

// mustChangePasswordExemptPaths are the only authenticated routes a user with
// an unsatisfied first-login password-change requirement may reach: the change
// endpoint itself and logout. Everything else is refused, so a known or default
// bootstrap credential cannot be used for anything but changing it.
var mustChangePasswordExemptPaths = map[string]bool{
	"/api/auth/password": true,
	"/api/auth/logout":   true,
}

// Native account flags restored from a snapshot confer no authentication authority.
// Either private marker triggers the hold, independently of runtime feature flags.
func (s *Server) requireAccountRestoreReleased(u users.User) error {
	if u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
		return sso.RequireNativeRestoreReleased(s.stateDir)
	}
	return nil
}
