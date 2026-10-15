package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/captcha"
)

func TestPoWChallengeLimiterAllowsBurstThenRefuses(t *testing.T) {
	l := newPowChallengeLimiter()
	now, advance := fixedClock(time.Now())
	l.now = now
	for i := 0; i < powChallengeBurst; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d of the burst should be allowed", i+1)
		}
	}
	if ok, _ := l.allow("1.2.3.4"); ok {
		t.Fatal("the request after the burst must be refused")
	}
	// A different client is unaffected: the endpoint is unauthenticated, so
	// one noisy IP must not deny service to everyone else.
	if ok, _ := l.allow("5.6.7.8"); !ok {
		t.Fatal("a different IP must have its own budget")
	}
	// A full window refills the whole burst.
	advance(powChallengeWindowLen + time.Second)
	for i := 0; i < powChallengeBurst; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d after a window should be allowed", i+1)
		}
	}
}

func TestPoWChallengeLimiterSweepDropsStaleWindows(t *testing.T) {
	l := newPowChallengeLimiter()
	now, advance := fixedClock(time.Now())
	l.now = now
	l.allow("1.2.3.4")
	if got := len(l.entries); got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
	advance(powChallengeWindowLen + time.Second)
	l.sweep()
	if got := len(l.entries); got != 0 {
		t.Fatalf("entries after sweep = %d, want 0", got)
	}
}

func TestPoWChallengeLimiterSweepsAtThreshold(t *testing.T) {
	// GET /api/auth/pow-challenge is unauthenticated and costs the caller
	// nothing, so an attacker presenting many distinct source IPs can mint
	// one map entry per request. sweep only runs on StartPoWSweeper's
	// 10-minute ticker, so without a second, size-triggered bound the map
	// could grow unbounded between ticks. allow() must reclaim refilled
	// buckets inline once the map crosses sweepThreshold.
	l := newPowChallengeLimiter()
	l.sweepThreshold = 4
	now, advance := fixedClock(time.Now())
	l.now = now

	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		l.allow(ip)
	}
	if got := len(l.entries); got != 4 {
		t.Fatalf("entries = %d, want 4", got)
	}

	// All four buckets have refilled, and the fifth insertion crosses the
	// (lowered) threshold, so it must trigger an inline sweep before adding
	// its own entry.
	advance(powChallengeWindowLen + time.Second)
	l.allow("5.5.5.5")

	if got := len(l.entries); got != 1 {
		t.Fatalf("entries after threshold sweep = %d, want 1 (only the new entry)", got)
	}
}

func TestResolvePoWSecretIsStableAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pow.key")
	first := resolvePoWSecret(path, nil)
	second := resolvePoWSecret(path, nil)
	if len(first) == 0 {
		t.Fatal("resolvePoWSecret returned an empty key")
	}
	if string(first) != string(second) {
		t.Fatal("the key must persist: a key that changes per call invalidates every in-flight challenge")
	}
}

func TestResolvePoWSecretPrefersTheEnvironment(t *testing.T) {
	t.Setenv("POW_SECRET", "operator-supplied-key")
	got := resolvePoWSecret(filepath.Join(t.TempDir(), "pow.key"), nil)
	if string(got) != "operator-supplied-key" {
		t.Fatalf("resolvePoWSecret() = %q, want the POW_SECRET value", got)
	}
}

func TestResolvePoWSecretRefusesAWeakEnvKey(t *testing.T) {
	// A short POW_SECRET is recoverable offline from a single issued
	// challenge, and whoever recovers it can mint their own challenge with
	// maxnumber 0 and a far-future expiry — the proof-of-work becomes a
	// silent no-op. Fail closed instead: NewVerifier rejects a nil key, so
	// login answers "captcha misconfigured" until an operator fixes it.
	t.Setenv("POW_SECRET", "changeme")
	if got := resolvePoWSecret(filepath.Join(t.TempDir(), "pow.key"), nil); got != nil {
		t.Fatalf("resolvePoWSecret() = %q, want nil for a %d-byte secret", got, len("changeme"))
	}
}

func TestResolvePoWSecretAcceptsAnEnvKeyAtTheMinimum(t *testing.T) {
	// The boundary is inclusive, so an operator who followed the documented
	// length is not rejected.
	key := "0123456789abcdef" // exactly powSecretMinLen bytes
	t.Setenv("POW_SECRET", key)
	if got := resolvePoWSecret(filepath.Join(t.TempDir(), "pow.key"), nil); string(got) != key {
		t.Fatalf("resolvePoWSecret() = %q, want %q", got, key)
	}
}

func TestResolvePoWSecretFallsBackToAnEphemeralKey(t *testing.T) {
	// A read-only secrets volume must not brick login on an install that
	// opted into pow. Challenges issued before a restart stop verifying,
	// which is a 5-minute annoyance; refusing to authenticate anyone is not.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := resolvePoWSecret(filepath.Join(blocker, "pow.key"), nil)
	if len(got) != 32 {
		t.Fatalf("len(resolvePoWSecret()) = %d, want a 32-byte ephemeral key", len(got))
	}
}

func TestPoWChallengeEndpointIssuesASignedChallenge(t *testing.T) {
	srv := newTestServer(t)
	v, err := captcha.NewPoWVerifier([]byte("test-key"), 100)
	if err != nil {
		t.Fatalf("NewPoWVerifier: %v", err)
	}
	srv.powVerifier = v

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/pow-challenge", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	srv.handlePoWChallenge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// A cached challenge is a replayed challenge.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var ch captcha.Challenge
	if err := json.Unmarshal(rec.Body.Bytes(), &ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if ch.Algorithm != "SHA-256" || ch.Salt == "" || ch.Challenge == "" || ch.Signature == "" {
		t.Fatalf("incomplete challenge: %+v", ch)
	}
	if ch.MaxNumber != 100 {
		t.Errorf("MaxNumber = %d, want 100", ch.MaxNumber)
	}
	// The challenge is bound to the address that asked for it, or an
	// escalated attacker just fetches cheap ones from a clean address.
	if ch.ClientIP != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want the requesting address", ch.ClientIP)
	}
	if ch.Expires <= time.Now().Unix() {
		t.Errorf("Expires = %d, want a future timestamp", ch.Expires)
	}
}

func TestPoWChallengeEndpointIs404WhenPoWIsOff(t *testing.T) {
	srv := newTestServer(t) // no powVerifier: the default install
	rec := httptest.NewRecorder()
	srv.handlePoWChallenge(rec, httptest.NewRequest(http.MethodGet, "/api/auth/pow-challenge", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when pow is not the configured provider", rec.Code)
	}
}

func TestPoWChallengeEndpointRateLimitsPerIP(t *testing.T) {
	srv := newTestServer(t)
	v, _ := captcha.NewPoWVerifier([]byte("test-key"), 100)
	srv.powVerifier = v

	// The endpoint is unauthenticated: without a limit it is a free
	// entropy-and-CPU faucet, and it feeds the spent-salt cache.
	var last *httptest.ResponseRecorder
	for i := 0; i < powChallengeBurst+1; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/pow-challenge", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		srv.handlePoWChallenge(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after the burst = %d, want 429", last.Code)
	}
	// One challenge refills every two seconds; the header rounds up.
	if got := last.Header().Get("Retry-After"); got != "2" && got != "3" {
		t.Errorf("Retry-After = %q, want 2 or 3", got)
	}
}

// stubVerifier lets these tests drive handleLogin's captcha branch directly,
// without solving a real challenge.
type stubVerifier struct {
	ok  bool
	err error
}

func (s stubVerifier) Verify(context.Context, string, string) (bool, error) { return s.ok, s.err }

func TestLoginRefundsTheStrikeForAnExpiredChallenge(t *testing.T) {
	srv := newTestServer(t)
	srv.captchaVerifier = stubVerifier{err: captcha.ErrChallengeExpired}

	// Get the bootstrap admin user's actual username
	all, err := srv.users.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("expected exactly one bootstrap user, got %+v err=%v", all, err)
	}
	adminUsername := all[0].Username

	// A stale tab is a clock, not a credential: three expired tabs must not
	// lock the user out. Loop loginMaxFailures + 1 times to prove the strikes
	// are refunded: if they were not, the final request would lock the account
	// and return 429; if they are refunded, it still returns 401.
	for i := 0; i <= loginMaxFailures; i++ {
		rec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
			map[string]string{"username": adminUsername, "password": "irrelevant", "captchaToken": "stale"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
}

func TestLoginRefundsTheStrikeForAChallengeIssuedToAnotherAddress(t *testing.T) {
	srv := newTestServer(t)
	srv.captchaVerifier = stubVerifier{err: captcha.ErrChallengeWrongClient}

	// Get the bootstrap admin user's actual username
	all, err := srv.users.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("expected exactly one bootstrap user, got %+v err=%v", all, err)
	}
	adminUsername := all[0].Username

	// A phone handing off between wifi and cellular can do this repeatedly
	// through no fault of its own, so a changed address must never lock an
	// account. Loop loginMaxFailures + 1 times to prove the strikes are
	// refunded: unrefunded, the final request would return 429.
	for i := 0; i <= loginMaxFailures; i++ {
		rec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
			map[string]string{"username": adminUsername, "password": "irrelevant", "captchaToken": "foreign"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
}

// A wrong solution checks no password and the budget refunds it, so it must not
// create per-account entries; it strikes the address only.
func TestWrongSolutionStrikesOnlyTheAddress(t *testing.T) {
	srv := newTestServer(t)
	srv.captchaVerifier = stubVerifier{ok: false}

	for i := 0; i <= loginMaxFailures; i++ {
		rec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
			map[string]string{"username": "admin", "password": "irrelevant", "captchaToken": "wrong"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	for l, want := range map[*failureLockout]int{srv.loginLockout: 0, srv.loginIPLockout: 1} {
		l.mu.Lock()
		n := len(l.entries)
		l.mu.Unlock()
		if n != want {
			t.Errorf("lockout holds %d entries after wrong solutions, want %d", n, want)
		}
	}
}

// countingVerifier rejects every token and counts provider calls.
type countingVerifier struct{ calls *int }

func (v countingVerifier) Verify(context.Context, string, string) (bool, error) {
	*v.calls++
	return false, nil
}

// Each provider call holds instance budget while in flight, so a locked address
// must be refused before the provider is called, and junk tokens must lock it.
func TestLockedAddressNeverReachesCaptchaProvider(t *testing.T) {
	srv := newTestServer(t)
	var calls int
	srv.captchaVerifier = countingVerifier{&calls}
	srv.loginIPLockout = newFailureLockout(3, loginIPLockoutFor)

	codes := make([]int, 5)
	for i := range codes {
		codes[i] = doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
			map[string]string{"username": fmt.Sprintf("u%d", i), "password": "x", "captchaToken": "junk"}).Code
	}
	if codes[2] != http.StatusUnauthorized || codes[3] != http.StatusTooManyRequests || codes[4] != http.StatusTooManyRequests {
		t.Fatalf("statuses = %v, want 401 x3 then 429", codes)
	}
	if calls != 3 {
		t.Errorf("provider called %d times, want 3: a locked address still reached it", calls)
	}
}

func TestLoginStillReports503WhenAProviderIsDown(t *testing.T) {
	srv := newTestServer(t)
	srv.captchaVerifier = stubVerifier{err: errors.New("siteverify unreachable")}

	// Get the bootstrap admin user's actual username
	all, err := srv.users.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("expected exactly one bootstrap user, got %+v err=%v", all, err)
	}
	adminUsername := all[0].Username

	rec := doJSON(srv, srv.handleLogin, http.MethodPost, "/api/auth/login",
		map[string]string{"username": adminUsername, "password": "irrelevant", "captchaToken": "x"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — the pre-existing outage path must be unchanged", rec.Code)
	}
}
