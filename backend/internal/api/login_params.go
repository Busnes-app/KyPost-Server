// The pre-login handshake that tells a browser how to derive its auth secret.
//
// This exists so the password never has to leave the browser: the client
// stretches it with the salt from here, splits the result into an authentication
// half and a key-wrapping half (see frontend/src/lib/authSecret.ts and
// keyVault.ts), and sends only the authentication half. POSTing the plaintext
// password on every sign-in while keyVault.ts derived the PGP vault key from
// that same password made the "the server cannot open your key" claim false by
// construction.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// handleLoginParams returns the salt and work factor a client must use to derive
// its auth secret for a given username. Public and unauthenticated: it runs
// before any credential exists.
//
// IT MUST NOT REVEAL WHETHER THE ACCOUNT EXISTS. A real account returns its
// stored salt; a nonexistent account, or a legacy one awaiting an admin reset,
// returns a synthetic salt derived from the instance secret and the username.
// All three are 16 random-looking bytes with no distinguishing shape, and the
// synthetic one is stable across requests, so asking twice does not out it
// either.
func (s *Server) handleLoginParams(w http.ResponseWriter, r *http.Request) {
	// Metered per IP, in requests. This endpoint performs no derivation, so charging
	// it a full attempt's reservation against the INSTANCE-WIDE derivation budget
	// priced a ~5us HMAC at 0.2 core-seconds: sixteen free requests emptied that
	// bucket and denied sign-in to every user, from any single address, with no
	// per-IP proxy rule able to restore it.
	if s.loginParamsLimiter != nil {
		if ok, _ := s.loginParamsLimiter.allow(lockoutKeyForIP(clientIP(r))); !ok {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": "too many sign-in attempts right now, try again shortly",
			})
			return
		}
	}

	username := strings.TrimSpace(r.URL.Query().Get("username"))
	// With no username, answer for the authenticated caller. That is what the
	// re-authentication flows need (step-up, disabling TOTP): they already have a
	// session, and making them echo their own username back invites a caller to
	// pass someone else's.
	if username == "" {
		if ac, ok := s.currentUser(r); ok {
			username = ac.Username
		}
	}
	salt, iterations := s.loginParamsFor(username)
	writeJSON(w, http.StatusOK, map[string]any{
		"salt":       salt,
		"iterations": iterations,
	})
}

// loginParamsFor resolves the salt and iteration count for a username, without
// disclosing whether it names a real account.
func (s *Server) loginParamsFor(username string) (salt string, iterations int) {
	if u, err := s.users.GetByUsername(username); err == nil && u.UsesDerivedAuth() && u.LoginSalt != "" {
		it := u.LoginIterations
		// Clamped at BOTH ends. The floor has always been here; the ceiling
		// matters because the client refuses to derive above
		// MaxLoginIterations, so serving a stored value past it hands the
		// browser a parameter it will reject and locks the account out of
		// itself. The store now rejects such a value on write — this covers
		// anything already persisted, and costs nothing.
		if it < users.MinLoginIterations || it > users.MaxLoginIterations {
			it = users.LoginIterations()
		}
		return u.LoginSalt, it
	}
	return s.syntheticLoginSalt(username), users.LoginIterations()
}

// syntheticLoginSalt derives a stable, per-username salt for any username this
// server cannot (or will not) hand a real one for. Keyed on the instance's
// pairing secret so it is unguessable without server access, and deterministic
// so repeated requests for the same username agree — a salt that changed per
// request would be a louder existence oracle than returning nothing at all.
//
// Credentials pinned to it by the retired legacy upgrade still depend on it, so
// the domain string and the fold must not change without a migration.
//
// Falls back to a fixed key when no pairing secret is configured: with no
// secret the salt is merely non-secret, and a salt's job is uniqueness, not
// secrecy.
func (s *Server) syntheticLoginSalt(username string) string {
	key := []byte(s.pairingSecret)
	if len(key) == 0 {
		key = []byte("kypost-login-salt-fallback")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("kypost/login-salt/v1|"))
	mac.Write([]byte(users.NormalizeUsername(username)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)[:16])
}
