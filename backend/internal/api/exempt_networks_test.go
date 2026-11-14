package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/captcha"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	officeRange = "203.0.113.0/24"
	officeIP    = "203.0.113.5" // inside officeRange
	outsideIP   = "198.51.100.9"
)

// officeServer is newTestServer with officeRange in RATE_LIMIT_EXEMPT_CIDRS.
func officeServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("RATE_LIMIT_EXEMPT_CIDRS", officeRange)
	return newTestServer(t)
}

// get serves one public GET from peer, with an optional X-Forwarded-For.
func exemptGet(srv *Server, path, peer, xff string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = peer + ":41000"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec.Code
}

// throttled reports whether n requests from peer met a 429.
func throttled(srv *Server, path, peer, xff string, n int) bool {
	for range n {
		if exemptGet(srv, path, peer, xff) == http.StatusTooManyRequests {
			return true
		}
	}
	return false
}

const loginParamsPath = "/api/auth/login-params?username=someone"

// An office goes past the per-IP login-params and PoW challenge budgets; an
// outside address is still refused at the same volume.
func TestExemptNetworkSkipsPerIPPreLoginLimits(t *testing.T) {
	srv := officeServer(t)
	v, err := captcha.NewPoWVerifier([]byte("test-hmac-key"), 5_000)
	if err != nil {
		t.Fatal(err)
	}
	srv.powVerifier = v
	for _, path := range []string{loginParamsPath, "/api/auth/pow-challenge"} {
		if throttled(srv, path, officeIP, "", 60) {
			t.Errorf("%s: the office was throttled", path)
		}
		if !throttled(srv, path, outsideIP, "", 60) {
			t.Errorf("%s: an outside address was not throttled", path)
		}
	}
	if got := srv.exemptNetworks.Load().Exempted(); got != 120 {
		t.Errorf("exempted = %d, want 120", got)
	}
}

// The SSO start route shares login-params' per-IP bucket; the office skips it.
func TestExemptNetworkSkipsTheSSOStartLimit(t *testing.T) {
	srv := officeServer(t)
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: "https://127.0.0.1:1", ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	srv.serverBaseURL = "https://mail.example.test"
	if throttled(srv, "/api/auth/oidc/login", officeIP, "", 40) {
		t.Error("the office was throttled starting SSO")
	}
	if !throttled(srv, "/api/auth/oidc/login", outsideIP, "", 40) {
		t.Error("an outside address was not throttled starting SSO")
	}
}

// With no trusted proxy, X-Forwarded-For is the caller's own claim and buys
// nothing. Behind a trusted proxy only the right-most hop counts, so a client
// prepending an office address to the chain is matched on its own address.
func TestExemptNetworkMatchesOnlyTheResolvedClient(t *testing.T) {
	trustProxyCIDRsForTest(t, "")
	srv := officeServer(t)
	if !throttled(srv, loginParamsPath, outsideIP, officeIP, 40) {
		t.Fatal("a forged X-Forwarded-For from an untrusted peer claimed the office network")
	}

	trustProxyCIDRsForTest(t, "192.0.2.0/24")
	srv = officeServer(t)
	if throttled(srv, loginParamsPath, "192.0.2.7", officeIP, 40) {
		t.Fatal("the office behind the trusted proxy was throttled")
	}
	if !throttled(srv, loginParamsPath, "192.0.2.7", officeIP+", "+outsideIP, 40) {
		t.Fatal("a client behind the trusted proxy claimed the office by prepending it")
	}
}

// The office skips the per-IP failed-sign-in lock; an outside address making
// the same failures is locked.
func TestExemptNetworkSkipsTheLoginIPLockout(t *testing.T) {
	srv := officeServer(t)
	srv.loginIPLockout = newFailureLockout(4, loginIPLockoutFor)
	srv.loginRateLimiter = nil // the instance budget has its own test below
	for i := range 10 {
		if code := loginFrom(t, srv, officeIP, "spray"+strconv.Itoa(i), "wrong-password-here"); code != http.StatusUnauthorized {
			t.Fatalf("office attempt %d answered %d, want 401", i+1, code)
		}
	}
	for i := range 4 {
		loginFrom(t, srv, outsideIP, "spray"+strconv.Itoa(i), "wrong-password-here")
	}
	if code := loginFrom(t, srv, outsideIP, "spray9", "wrong-password-here"); code != http.StatusTooManyRequests {
		t.Fatalf("outside address after 4 failures answered %d, want 429", code)
	}
}

// The per-account lockout still applies to the office: three wrong guesses at
// one account lock it there, and the right password is then refused too.
func TestExemptNetworkKeepsThePerAccountLockout(t *testing.T) {
	srv := officeServer(t)
	if _, err := srv.users.Create(t.Context(), "carol", "carol-office-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	for i := range loginMaxFailures {
		if code := loginFrom(t, srv, officeIP, "carol", "wrong-password-here"); code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d", i+1, code)
		}
	}
	rec := loginAttempt(srv, "carol", "carol-office-password", officeIP+":1234")
	if rec.Code != http.StatusTooManyRequests || strings.Contains(rec.Body.String(), "from this address") {
		t.Fatalf("locked account from the office answered %d %s, want the per-account 429", rec.Code, rec.Body)
	}
}

// The instance-wide derivation budget still applies to the office.
func TestExemptNetworkKeepsTheInstanceBudget(t *testing.T) {
	srv := officeServer(t)
	resetInstanceBudget(t, srv)
	for i := range loginRateBurst + 5 {
		rec := loginAttempt(srv, "budget"+strconv.Itoa(i), "wrong-password-here", officeIP+":1234")
		if rec.Code == http.StatusTooManyRequests {
			if !strings.Contains(rec.Body.String(), "right now") {
				t.Fatalf("attempt %d: 429 from the wrong limiter: %s", i+1, rec.Body)
			}
			return
		}
	}
	t.Fatal("the office was never stopped by the instance-wide login budget")
}

// exemptAdmin is an admin server whose root state.db is stateDir, so a second
// NewServer with the same STATE_DIR is a restart.
func exemptAdmin(t *testing.T, stateDir string) (srv *Server, put func(networks string, confirm bool, as string) *httptest.ResponseRecorder, adminID, memberID string) {
	t.Helper()
	t.Setenv("STATE_DIR", stateDir)
	srv, admin, member := adminAndMember(t)
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	srv.globalStore = st
	put = func(networks string, confirm bool, as string) *httptest.ResponseRecorder {
		body := map[string]any{"networks": networks}
		if confirm {
			body["adminAuthSecret"] = authSecretFor(srv, "session-tester-testpassword")
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPut, "/api/admin/rate-limit-networks", bytes.NewReader(raw))
		authRequestAs(srv, req, as)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	return srv, put, admin.ID, member.ID
}

func exemptStatus(t *testing.T, rec *httptest.ResponseRecorder) (networks, environment string, exempted float64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Networks, Environment string
		Exempted              float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Networks, out.Environment, out.Exempted
}

// An admin's list is step-up confirmed, stored canonically, live at once,
// audited, kept across a restart alongside the environment's, and clearable.
func TestAdminSetsExemptNetworks(t *testing.T) {
	t.Setenv("RATE_LIMIT_EXEMPT_CIDRS", "198.18.7.0/24")
	stateDir := t.TempDir()
	srv, put, adminID, memberID := exemptAdmin(t, stateDir)
	var logs bytes.Buffer
	logger, err := logging.NewWithOutput(&logs)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger
	read := func(as string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/rate-limit-networks", nil)
		authRequestAs(srv, req, as)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}

	if code := read(memberID).Code; code != http.StatusForbidden {
		t.Fatalf("member GET answered %d, want 403", code)
	}
	if code := put(officeRange, true, memberID).Code; code != http.StatusForbidden {
		t.Fatalf("member PUT answered %d, want 403", code)
	}
	if code := put(officeRange, false, adminID).Code; code != http.StatusForbidden {
		t.Fatalf("PUT without step-up answered %d, want 403", code)
	}
	if networks, env, _ := exemptStatus(t, read(adminID)); networks != "" || env != "198.18.7.0/24" {
		t.Fatalf("before: networks=%q environment=%q", networks, env)
	}
	if !throttled(srv, loginParamsPath, officeIP, "", 40) {
		t.Fatal("the office was exempt before any change")
	}

	networks, _, _ := exemptStatus(t, put(" 203.0.113.77/24 , 2001:db8:1::/48", true, adminID))
	if networks != "203.0.113.0/24,2001:db8:1::/48" {
		t.Fatalf("stored %q, want the canonical list", networks)
	}
	if throttled(srv, loginParamsPath, "203.0.113.6", "", 40) || throttled(srv, loginParamsPath, "198.18.7.50", "", 40) {
		t.Fatal("an allowlisted network was throttled after the change")
	}
	if _, _, exempted := exemptStatus(t, read(adminID)); exempted != 80 {
		t.Fatalf("exempted = %v, want 80", exempted)
	}
	if !strings.Contains(logs.String(), `"action":"set_exempt_networks"`) || !strings.Contains(logs.String(), `"actor":"`+adminID+`"`) ||
		!strings.Contains(logs.String(), `to=\"203.0.113.0/24,2001:db8:1::/48\"`) {
		t.Fatalf("no audit record for the change: %s", logs.String())
	}

	// Restart: a new server on the same state reads the stored list back.
	restarted := newTestServer(t)
	if throttled(restarted, loginParamsPath, "203.0.113.7", "", 40) || throttled(restarted, loginParamsPath, "198.18.7.51", "", 40) {
		t.Fatal("the stored list did not survive a restart")
	}

	// Clearing restores the limit for the office; the environment's stays exempt.
	if networks, _, _ := exemptStatus(t, put("", true, adminID)); networks != "" {
		t.Fatalf("cleared list reads %q", networks)
	}
	if !throttled(srv, loginParamsPath, "203.0.113.200", "", 40) {
		t.Fatal("a removed network is still exempt")
	}
	if throttled(srv, loginParamsPath, "198.18.7.52", "", 40) {
		t.Fatal("clearing the admin list dropped the environment's network")
	}
}

// A list that would switch the limit off widely is refused, logged, and
// nothing changes.
func TestExemptNetworksRefuseBroadOrInvalidRanges(t *testing.T) {
	srv, put, adminID, _ := exemptAdmin(t, t.TempDir())
	if rec := put(officeRange, true, adminID); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/8", "2001:db8::/32", "office", "127.0.0.1", "::1", officeRange + ",nope"} {
		if rec := put(bad, true, adminID); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q answered %d, want 400", bad, rec.Code)
		}
	}
	if rec := put("10.0.0.0/8", true, adminID); !strings.Contains(rec.Body.String(), "too broad") {
		t.Errorf("refusal does not say why: %s", rec.Body)
	}
	if stored, _ := srv.storedExemptNetworks(); stored != officeRange {
		t.Fatalf("a refused list replaced the stored one: %q", stored)
	}
	if !throttled(srv, loginParamsPath, "10.9.9.9", "", 40) {
		t.Fatal("an address in a refused range was exempted")
	}
}

// A stored list that no longer parses is dropped at startup for the
// environment's list: fewer exemptions, never more.
func TestUnreadableStoredExemptNetworksAreDropped(t *testing.T) {
	stateDir := t.TempDir()
	st, err := state.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(exemptNetworksSetting, "10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("RATE_LIMIT_EXEMPT_CIDRS", "198.18.7.0/24")
	srv := newTestServer(t)
	if got := srv.exemptNetworks.Load().String(); got != "198.18.7.0/24" {
		t.Fatalf("live list %q, want the environment's only", got)
	}
}

// With nothing configured nothing is exempt.
func TestNoExemptNetworksByDefault(t *testing.T) {
	t.Setenv("RATE_LIMIT_EXEMPT_CIDRS", "")
	srv := newTestServer(t)
	if srv.exemptNetworks.Load().Len() != 0 || !throttled(srv, loginParamsPath, officeIP, "", 40) {
		t.Fatal("an address was exempt with no list configured")
	}
}

// freezeLimiters stops the refill of each limiter, so a test can count a
// budget exactly however long the run takes.
func freezeLimiters(ls ...*keyedLimiter) {
	at := time.Now()
	for _, l := range ls {
		l.now = func() time.Time { return at }
	}
}

// Offices share one budget per limit. Past it their requests meet the
// ordinary per-IP limit again, so one host inside an office cannot make
// unbounded requests; another office address still has its own per-IP limit.
func TestOfficeBudgetFallsBackToThePerIPLimit(t *testing.T) {
	srv := officeServer(t)
	freezeLimiters(srv.officeLoginParams, srv.loginParamsLimiter)
	for i := range officeRequestsPerMinute + loginParamsBurst {
		if code := exemptGet(srv, loginParamsPath, officeIP, ""); code != http.StatusOK {
			t.Fatalf("office request %d answered %d, want the office budget and then the per-IP burst", i+1, code)
		}
	}
	if code := exemptGet(srv, loginParamsPath, officeIP, ""); code != http.StatusTooManyRequests {
		t.Fatalf("past both budgets the office answered %d, want 429", code)
	}
	if code := exemptGet(srv, loginParamsPath, "203.0.113.77", ""); code != http.StatusOK {
		t.Fatalf("a second office address answered %d: throttled by the first one's per-IP limit", code)
	}
	// The SSO meter draws on the same spent office budget.
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: "https://127.0.0.1:1", ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	srv.serverBaseURL = "https://mail.example.test"
	if code := exemptGet(srv, "/api/auth/oidc/login", officeIP, ""); code != http.StatusTooManyRequests {
		t.Fatalf("SSO start past both budgets answered %d, want 429", code)
	}
	// Exempted counts only requests that skipped the per-IP limit.
	if got := srv.exemptNetworks.Load().Exempted(); got != officeRequestsPerMinute {
		t.Fatalf("exempted = %d, want %d", got, officeRequestsPerMinute)
	}
}

// The same for the PoW challenge budget.
func TestOfficePoWBudgetFallsBackToThePerIPLimit(t *testing.T) {
	srv := officeServer(t)
	v, err := captcha.NewPoWVerifier([]byte("test-hmac-key"), 5_000)
	if err != nil {
		t.Fatal(err)
	}
	srv.powVerifier = v
	freezeLimiters(srv.officePoW, srv.powChallenges)
	const path = "/api/auth/pow-challenge"
	for i := range officeRequestsPerMinute + powChallengeBurst {
		if code := exemptGet(srv, path, officeIP, ""); code != http.StatusOK {
			t.Fatalf("office challenge %d answered %d", i+1, code)
		}
	}
	if code := exemptGet(srv, path, officeIP, ""); code != http.StatusTooManyRequests {
		t.Fatalf("past both budgets the office answered %d, want 429", code)
	}
	if code := exemptGet(srv, path, "203.0.113.77", ""); code != http.StatusOK {
		t.Fatalf("a second office address answered %d", code)
	}
}

// Sign-in failures from offices draw on one office budget standing in for the
// address lock. Past it the address lock applies again, and the instance-wide
// login budget applies throughout.
func TestOfficeLoginBudgetFallsBackToTheAddressLock(t *testing.T) {
	srv := officeServer(t)
	srv.loginIPLockout = newFailureLockout(4, loginIPLockoutFor)
	srv.loginRateLimiter = nil // the instance budget has its own part below
	freezeLimiters(srv.officeLogin)
	for i := range officeLoginBurst + 4 {
		if code := loginFrom(t, srv, officeIP, "spray"+strconv.Itoa(i), "wrong-password-here"); code != http.StatusUnauthorized {
			t.Fatalf("office failure %d answered %d, want 401", i+1, code)
		}
	}
	rec := loginAttempt(srv, "spray-next", "wrong-password-here", officeIP+":1234")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "from this address") {
		t.Fatalf("past the office budget answered %d %s, want the address lock", rec.Code, rec.Body)
	}
	if code := loginFrom(t, srv, "203.0.113.77", "spray-other", "wrong-password-here"); code != http.StatusUnauthorized {
		t.Fatalf("a second office address answered %d: locked by the first one's failures", code)
	}

	// The instance budget still stops an office whose own budget is untouched.
	srv = officeServer(t)
	resetInstanceBudget(t, srv)
	freezeLimiters(srv.loginRateLimiter, srv.officeLogin)
	for i := range loginRateBurst + 1 {
		rec := loginAttempt(srv, "budget"+strconv.Itoa(i), "wrong-password-here", officeIP+":1234")
		if rec.Code == http.StatusTooManyRequests {
			if !strings.Contains(rec.Body.String(), "right now") {
				t.Fatalf("attempt %d: 429 from the wrong limiter: %s", i+1, rec.Body)
			}
			return
		}
	}
	t.Fatal("the office was never stopped by the instance-wide login budget")
}

// A correct sign-in hands its office reservation back: only failures spend it.
func TestSuccessfulOfficeSignInsDoNotSpendTheOfficeBudget(t *testing.T) {
	srv := officeServer(t)
	srv.loginIPLockout = newFailureLockout(1, loginIPLockoutFor)
	srv.loginRateLimiter = nil
	freezeLimiters(srv.officeLogin)
	if _, err := srv.users.Create(t.Context(), "dana", "dana-office-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	for i := range officeLoginBurst + 5 {
		if rec := loginAttempt(srv, "dana", "dana-office-password", officeIP+":1234"); rec.Code != http.StatusOK {
			t.Fatalf("sign-in %d answered %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if ok, _ := srv.officeLogin.allow(officeBudgetKey); !ok || srv.officeLogin.entries[officeBudgetKey].tokens != officeLoginBurst-1 {
		t.Fatal("successful sign-ins spent the office budget")
	}
}

// Concurrent wrong guesses at one account from an office reserve their place
// in the account lockout before any verification: however they interleave, no
// more than loginMaxFailures reach the password check.
func TestConcurrentOfficeGuessesCannotOutrunTheAccountLockout(t *testing.T) {
	srv := officeServer(t)
	srv.loginRateLimiter = nil
	if _, err := srv.users.Create(t.Context(), "erin", "erin-office-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	const guesses = 30
	codes := make([]int, guesses)
	var wg sync.WaitGroup
	for i := range guesses {
		wg.Go(func() {
			codes[i] = loginAttempt(srv, "erin", "wrong-password-here", officeIP+":1234").Code
		})
	}
	wg.Wait()
	verified := 0
	for _, code := range codes {
		switch code {
		case http.StatusUnauthorized: // the only 401 past the lockout is a failed verification
			verified++
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if verified > loginMaxFailures {
		t.Fatalf("%d guesses reached verification, want at most %d", verified, loginMaxFailures)
	}
}

// The office budgets are sized against what they stand beside: the sign-in
// budget never admits more than the instance login budget, and an office
// failing at full rate for a whole lockout window cannot reach the size at
// which the per-account lockout table sweeps, let alone evicts.
func TestOfficeBudgetsFitTheLimiters(t *testing.T) {
	if officeLoginBurst > loginRateBurst || officeLoginRefillPerSec > loginKDFDutyCycle/loginKDFReserveSeconds {
		t.Fatal("the office sign-in budget exceeds the instance login budget")
	}
	perWindow := officeLoginBurst + officeLoginRefillPerSec*loginLockoutFor.Seconds() + loginIPMaxFailures
	if perWindow >= loginLockoutSweepThreshold/10 {
		t.Fatalf("an office can write %v lockout keys per window, want under %d", perWindow, loginLockoutSweepThreshold/10)
	}
}
