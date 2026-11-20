package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/health"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/wkdpublish"
)

// replicaOf is a second server on a's volumes and database, as another container would be:
// its own users store, state handle and in-memory structures over the same files.
func replicaOf(t *testing.T, a *Server) *Server {
	t.Helper()
	logger, err := logging.New()
	if err != nil {
		t.Fatal(err)
	}
	usersStore, err := users.LoadOrMigrate(context.Background(), a.configDir, filepath.Join(a.configDir, "admin.env"))
	if err != nil {
		t.Fatal(err)
	}
	wkd, err := wkdpublish.New(a.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	b := NewServer(config.Default(), logger, health.NewService(), usersStore, nil, wkd)
	b.pairingSecret = a.pairingSecret
	b.stateDir, b.configDir, b.configPath = a.stateDir, a.configDir, a.configPath
	b.ssoStore = sso.NewStore(a.configDir)
	b.nativeDomains = sso.NewNativeDomainStore(a.configDir)
	b.ssoLifecycle = sso.NewLifecycleStore(a.configDir)
	b.totpSecretKeyPath, b.pgpPrivateKeyPath = a.totpSecretKeyPath, a.pgpPrivateKeyPath
	b.pickupStore = pgpmail.NewPickupStore(filepath.Join(a.stateDir, "pickup"), filepath.Join(a.configDir, "pickup-store.key"))
	gs, err := state.New(a.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	b.globalStore = gs
	return b
}

// sharedPair is two replicas on one database with shared security state.
func sharedPair(t *testing.T) (*Server, *Server) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	a := newTestServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	b := replicaOf(t, a)
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	return a, b
}

func loginCookies(t *testing.T, srv *Server, username, password string) []*http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "authSecret": testAuthSecret(srv, username, password)})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.20:1234"
	rec := httptest.NewRecorder()
	srv.handleLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()
}

func cookieValue(cs []*http.Cookie, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func send(srv *Server, method, path string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "198.51.100.20:1234"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func authenticated(t *testing.T, srv *Server, cookies []*http.Cookie) bool {
	t.Helper()
	rec := send(srv, http.MethodGet, "/api/auth/me", cookies, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/auth/me: %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		Authenticated *bool `json:"authenticated"`
		ID            string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	return me.Authenticated == nil || *me.Authenticated
}

// AC4: a session made on A authorizes on B, CSRF included; logout, revocation and the SSO
// epoch bump on A are refused on B on the next request; only token hashes are stored.
func TestSessionsAreSharedAcrossReplicas(t *testing.T) {
	a, b := sharedPair(t)
	if _, err := a.users.Create(context.Background(), "alice", "alice-test-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, a, "alice", "alice-test-password")
	token, csrf := cookieValue(cookies, "kypost_session"), cookieValue(cookies, "csrf_token")
	if !authenticated(t, b, cookies) {
		t.Fatal("a session made on A does not authorize on B")
	}
	if rec := send(b, http.MethodPost, "/api/auth/logout", cookies, "wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("B accepted a wrong CSRF token: %d", rec.Code)
	}
	// The table holds only hashes: no row carries the cookie value.
	db, err := dbbackend.OpenFile(filepath.Join(a.stateDir, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hits int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE token_hash = ? OR data LIKE ? OR user_id = ?`, token, "%"+token+"%", token).Scan(&hits); err != nil || hits != 0 {
		t.Fatalf("a session row carries the token: %d %v", hits, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE token_hash = ?`, sessionHash(token)).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("no row under the token's hash: %d %v", hits, err)
	}

	// Logout on B (CSRF from A's login passes on B) ends it on A.
	if rec := send(b, http.MethodPost, "/api/auth/logout", cookies, csrf); rec.Code != http.StatusOK {
		t.Fatalf("logout on B: %d %s", rec.Code, rec.Body.String())
	}
	if authenticated(t, a, cookies) {
		t.Fatal("a logout on B left the session alive on A")
	}

	// A revocation (password change, MFA change, admin reset) on A ends B's view.
	cookies = loginCookies(t, a, "alice", "alice-test-password")
	keep := loginCookies(t, b, "alice", "alice-test-password")
	u, _ := a.users.GetByUsername("alice")
	if err := a.revokeUserSessions(u.ID, cookieValue(keep, "kypost_session")); err != nil {
		t.Fatal(err)
	}
	if authenticated(t, b, cookies) {
		t.Fatal("a revocation on A left the session alive on B")
	}
	if !authenticated(t, a, keep) {
		t.Fatal("the revocation ended the session it spared")
	}

	// A sign-in whose fence was read before a revocation on the other replica is refused.
	mark, err := b.revocationMark(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.revokeUserSessions(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.putSharedSession(context.Background(), "late-token", Session{UserID: u.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), revocations: mark + 1}); err == nil {
		t.Fatal("B minted a session across a revocation on A")
	}

	// The SSO epoch: a bump on A ends B's SSO sessions and refuses an SSO mint fenced before it.
	epoch, _ := b.currentSSOEpoch(context.Background())
	ssoSess := Session{UserID: u.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), SSOEpoch: epoch, SSO: sso.SessionIdentity{Subject: "sub-1", Issuer: "https://idp"}}
	if err := b.putSharedSession(context.Background(), "sso-token", ssoSess); err != nil {
		t.Fatal(err)
	}
	if err := a.endSSOSessions(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.lookupSession(context.Background(), "sso-token"); ok {
		t.Fatal("the SSO epoch bump on A left an SSO session on B")
	}
	if err := b.putSharedSession(context.Background(), "sso-late", ssoSess); err == nil {
		t.Fatal("B minted an SSO session fenced before the bump on A")
	}
}

// Without UseSharedState (SQLite, and every server a test builds by default) nothing is
// written to the shared tables.
func TestMemoryServersWriteNoSharedRows(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.users.Create(context.Background(), "bob", "bob-test-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, srv, "bob", "bob-test-password")
	if !authenticated(t, srv, cookies) {
		t.Fatal("login failed")
	}
	for range 4 {
		loginFrom(t, srv, "203.0.113.5", "bob", "wrong-password")
	}
	db, err := dbbackend.OpenFile(filepath.Join(srv.stateDir, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"sessions", "security_windows", "spent_challenges", "replay_ids", "auth_challenges", "session_revocations", "import_jobs", "replica_instances"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", table, n, err)
		}
	}
}

// AC7: each shared lockout and budget admits exactly its limit across two servers under
// concurrency, never twice it.
func TestLockoutsAreSharedAcrossReplicas(t *testing.T) {
	a, b := sharedPair(t)
	pairs := map[string][2]*failureLockout{
		"login":           {a.loginLockout, b.loginLockout},
		"login-ip":        {a.loginIPLockout, b.loginIPLockout},
		"review-pairing":  {a.reviewPairingLockout, b.reviewPairingLockout},
		"dav":             {a.davLockout, b.davLockout},
		"mcp-ip":          {a.mcpLockout, b.mcpLockout},
		"mcp-user":        {a.mcpUserLockout, b.mcpUserLockout},
		"mfa":             {a.mfaLockout, b.mfaLockout},
		"device":          {a.deviceLockout, b.deviceLockout},
		"password-change": {a.passwordChangeLockout, b.passwordChangeLockout},
		"imap-login":      {a.imapLoginLockout, b.imapLoginLockout},
	}
	for name, p := range pairs {
		limit := p[0].maxFailures
		var admitted atomic.Int32
		var wg sync.WaitGroup
		for i := range 2*limit + 4 {
			wg.Go(func() {
				if ok, _, _ := p[i%2].tryAttempt("k"); ok {
					admitted.Add(1)
				}
			})
		}
		wg.Wait()
		if got := int(admitted.Load()); got != limit {
			t.Errorf("%s: admitted %d across two replicas, want exactly %d", name, got, limit)
		}
		if p[1].lockedFor("k") <= 0 {
			t.Errorf("%s: B does not see the lockout A and B filled", name)
		}
		p[0].recordSuccess("k")
		if p[1].lockedFor("k") != 0 {
			t.Errorf("%s: a success on A did not clear B's view", name)
		}
	}
	for name, p := range map[string][2]*keyedLimiter{
		"login-kdf":    {a.loginRateLimiter, b.loginRateLimiter},
		"office-login": {a.officeLogin, b.officeLogin},
		"office-pow":   {a.officePoW, b.officePoW},
	} {
		cost := 1.0
		if name == "login-kdf" {
			cost = loginKDFReserveSeconds
		}
		limit, _ := p[0].sharedWindow(cost)
		var admitted atomic.Int32
		var wg sync.WaitGroup
		for i := range 2*limit + 4 {
			wg.Go(func() {
				if ok, _, _ := p[i%2].reserve("k", cost); ok {
					admitted.Add(1)
				}
			})
		}
		wg.Wait()
		if got := int(admitted.Load()); got != limit {
			t.Errorf("%s: admitted %d across two replicas, want exactly %d", name, got, limit)
		}
	}
}

// A refund hands back only the reservation it took, and a failing counter store denies.
func TestSharedLockoutRefundAndFailure(t *testing.T) {
	a, b := sharedPair(t)
	ok, _, res := a.davLockout.tryAttempt("ip")
	if !ok {
		t.Fatal("first attempt refused")
	}
	for range davMaxFailures - 1 {
		if ok, _, _ := b.davLockout.tryAttempt("ip"); !ok {
			t.Fatal("refused under the limit")
		}
	}
	if ok, _, _ := b.davLockout.tryAttempt("ip"); ok {
		t.Fatal("admitted past the limit")
	}
	a.davLockout.cancelAttempt(res)
	if ok, _, _ := b.davLockout.tryAttempt("ip"); !ok {
		t.Fatal("A's refund did not free one attempt on B")
	}
	if ok, _, _ := b.davLockout.tryAttempt("ip"); ok {
		t.Fatal("one refund freed two attempts")
	}

	// The shared store fails: every lockout denies and says it was unavailable.
	_ = b.shared.Close()
	ok, _, res = b.davLockout.tryAttempt("other")
	if ok || !res.unavailable {
		t.Fatalf("a failing counter store admitted (ok=%v unavailable=%v)", ok, res.unavailable)
	}
	if ok, _, _ := b.loginRateLimiter.reserve(loginRateLimitKey, loginKDFReserveSeconds); ok {
		t.Fatal("a failing counter store admitted a login KDF reservation")
	}
}

// AC6: with the shared store failing, session routes answer 503 unavailable and never 401;
// DAV and MCP answer 503 with Retry-After; a bad credential still answers 401.
func TestStoreFailureIsUnavailableNotSignedOut(t *testing.T) {
	a, b := sharedPair(t)
	if _, err := a.users.Create(context.Background(), "carol", "carol-test-password", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, a, "carol", "carol-test-password")
	bad := []*http.Cookie{{Name: "kypost_session", Value: "not-a-session"}}
	if rec := send(b, http.MethodGet, "/api/config", bad, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie: %d", rec.Code)
	}
	_ = b.shared.Close()
	for _, path := range []string{"/api/config", "/api/auth/me", "/api/auth/csrf", "/api/inbox"} {
		rec := send(b, http.MethodGet, path, cookies, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
			t.Errorf("%s with the store down: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{davPrefix + "/", "/mcp"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "198.51.100.30:1234"
		if path == "/mcp" {
			req.Header.Set("Authorization", "Bearer kpa_x_y")
		} else {
			req.SetBasicAuth("carol", "app-password")
		}
		rec := httptest.NewRecorder()
		b.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s with the store down: %d Retry-After=%q", path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
}

// AC18: a settings PUT on A is served by B within 5 s; an office network removed on A stops
// exempting on B within 5 s.
func TestSettingsFollowTheVolumeAndDatabase(t *testing.T) {
	a, b := sharedPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.StartConfigFollower(ctx)
	admin, err := a.users.Create(context.Background(), "settings-admin", "settings-admin-password", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(a, admin.ID)
	cookies := []*http.Cookie{{Name: "kypost_session", Value: token}}
	rec := send(a, http.MethodGet, "/api/config", cookies, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config: %d %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["timezone"] = "Europe/Lisbon"
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req.Header.Set("X-CSRF-Token", csrf)
	rec = httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT config on A: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(followInterval + time.Second)
	for {
		b.cfgMu.RLock()
		tz := b.cfg.Timezone
		b.cfgMu.RUnlock()
		if tz == "Europe/Lisbon" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B still serves timezone %q after %v", tz, followInterval+time.Second)
		}
		time.Sleep(100 * time.Millisecond)
	}

	office := netip.MustParseAddr("192.0.2.10")
	if err := a.globalStore.SetSetting(exemptNetworksSetting, "192.0.2.0/24"); err != nil {
		t.Fatal(err)
	}
	waitFor := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(followInterval + time.Second)
		for b.exemptNetworks.Load().Contains(office) != want {
			if time.Now().After(deadline) {
				t.Fatalf("B's office list does not reflect A's change (want contains=%v)", want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitFor(true)
	if err := a.globalStore.SetSetting(exemptNetworksSetting, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(false)
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = office.String() + ":1234"
	if b.officeDraw(req, b.officeLogin) {
		t.Fatal("B still exempts a removed office network")
	}
}

// AC9: a CardDAV app password revoked on A is refused on B on the next request, although B
// verified (and cached) it a moment before.
func TestDAVRevocationReachesEveryReplica(t *testing.T) {
	a, b := sharedPair(t)
	u, err := a.users.Create(context.Background(), "dav-replica", "dav-replica-password", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	const appPassword = "app-specific-password-replica"
	hash, err := users.HashPassword(context.Background(), appPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.writeDAVPassword(u.ID, davPasswordFile{Hash: hash, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	dav := func(srv *Server) int {
		reached := false
		handler := srv.withDAVBasicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(207) }))
		req := httptest.NewRequest("PROPFIND", davPrefix+"/dav-replica/", nil)
		req.SetBasicAuth(u.Username, appPassword)
		req.RemoteAddr = "203.0.113.40:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if reached {
			return 207
		}
		return rec.Code
	}
	if got := dav(b); got != 207 {
		t.Fatalf("B before revocation: %d", got)
	}
	if got := dav(b); got != 207 {
		t.Fatalf("B from its cache: %d", got)
	}
	if err := a.revokeAllUserCredentials(u); err != nil {
		t.Fatal(err)
	}
	if got := dav(b); got != http.StatusUnauthorized {
		t.Fatalf("B accepted an app password A revoked: %d", got)
	}
}

// The session row is a compare-and-swap: an SSO link grant held in the session is spent on
// exactly one replica, and two replicas changing different fields of one session at once both
// land (the loser re-reads and re-applies).
func TestSessionChangesAreCompareAndSwap(t *testing.T) {
	a, b := sharedPair(t)
	user, err := a.users.Create(context.Background(), "cas-user", "long-password-for-cas", users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := mintSessionForTest(a, user.ID)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		return r
	}
	for round := range 10 {
		flow := fmt.Sprintf("state-%d", round)
		if !a.grantSSOLink(req(), flow) {
			t.Fatal("grant", round)
		}
		var spent atomic.Int32
		var wg sync.WaitGroup
		for _, srv := range []*Server{a, b} {
			wg.Go(func() {
				if srv.consumeSSOLinkGrant(req(), flow) {
					spent.Add(1)
				}
			})
		}
		wg.Wait()
		if spent.Load() != 1 {
			t.Fatalf("round %d: one SSO link grant was spent %d times", round, spent.Load())
		}
	}
	for round := range 10 {
		at := time.Now().Add(time.Duration(round) * time.Second).Truncate(time.Millisecond)
		var wg sync.WaitGroup
		var wrote atomic.Int32
		change := func(srv *Server, f func(*Session)) {
			ok, err := srv.changeSession(context.Background(), token, func(s *Session) bool { f(s); return true })
			if err == nil && ok {
				wrote.Add(1)
			}
		}
		wg.Go(func() { change(a, func(s *Session) { s.SteppedUpAt = at }) })
		wg.Go(func() { change(b, func(s *Session) { s.SSOLinkState = "touched-" + at.String() }) })
		wg.Wait()
		sess, ok, err := b.lookupSession(context.Background(), token)
		if err != nil || !ok || wrote.Load() != 2 || !sess.SteppedUpAt.Equal(at) || sess.SSOLinkState != "touched-"+at.String() {
			t.Fatalf("round %d: concurrent changes lost one: wrote=%d steppedUp=%v state=%q", round, wrote.Load(), sess.SteppedUpAt, sess.SSOLinkState)
		}
	}
}

// A device approval on one replica and a credential revocation (a password change) on
// another: whichever runs first, no approved device credential survives the revocation.
// The approval is held after its subscriber check and before its device write while the
// revocation runs; with only a per-process lock the revocation finished in that gap and the
// device was written after it.
func TestApprovalCannotOutliveARevocationOnAnotherReplica(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, u := newPendingServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	b := replicaOf(t, a)
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	_, reg := webPairAndRegister(t, a, u.ID, "phone-race")
	if reg.Status != "pending" {
		t.Fatalf("registration: %+v", reg)
	}
	checked, proceed := make(chan struct{}), make(chan struct{})
	a.nativeApproveBeforePersist = func() { close(checked); <-proceed }
	approved := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/notifications/native/pending/"+reg.DeviceID+"/approve", nil)
		authRequestAs(a, req, u.ID)
		rec := httptest.NewRecorder()
		a.routes().ServeHTTP(rec, req)
		approved <- rec.Code
	}()
	<-checked
	revoked := make(chan error, 1)
	go func() { revoked <- b.revokeAllUserCredentials(u) }()
	// Without the shared lock the revocation completes here, inside the approval's gap.
	select {
	case err := <-revoked:
		revoked <- err
	case <-time.After(3 * time.Second): // waiting for the approval's lock
	}
	close(proceed)
	<-approved
	if err := <-revoked; err != nil {
		t.Fatal("revocation:", err)
	}
	for name, srv := range map[string]*Server{"A": a, "B": b} {
		if rec := deviceCall(srv, "/api/notifications/native/pull", reg.DeviceID, reg.DeviceSecret); rec.Code == http.StatusOK {
			t.Fatalf("the device approved during a revocation still works on %s", name)
		}
	}
}

// ssoPair is two replicas sharing SSO settings, sessions and the provider; ssoSessionsEndFault
// fails A's first attempt to end sessions.
func ssoPair(t *testing.T) (*Server, *Server, *ssotest.IdP) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	a, idp := setupSSOTestServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	b := replicaOf(t, a)
	b.serverBaseURL = a.serverBaseURL
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	var calls atomic.Int32
	a.ssoSessionsEndFault = func() error {
		if calls.Add(1) == 1 {
			return errors.New("injected: the database dropped the connection")
		}
		return nil
	}
	return a, b, idp
}

// A back-channel logout whose session deletion fails is not consumed: the provider's retry
// ends the covered session on every replica.
func TestFailedBackchannelLogoutIsRetried(t *testing.T) {
	a, b, idp := ssoPair(t)
	c := ssoSignIn(t, a, idp, "alice", "sid-retry")
	token := idp.LogoutToken(t, "alice", "sid-retry", nil)
	if rec := postLogout(a, logoutForm(token)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout with a failing deletion: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postLogout(a, logoutForm(token)); rec.Code != http.StatusOK {
		t.Fatalf("the provider's retry: %d %s", rec.Code, rec.Body.String())
	}
	for name, srv := range map[string]*Server{"A": a, "B": b} {
		if sessionAlive(srv, c) {
			t.Fatalf("the logged-out session still works on %s", name)
		}
	}
}

// Disabling SSO when ending its sessions fails changes nothing; saving again disables it and
// ends them on every replica.
func TestFailedSSODisableIsRetried(t *testing.T) {
	a, b, idp := ssoPair(t)
	c := ssoSignIn(t, a, idp, "alice", "sid-disable")
	admin, err := a.users.Create(context.Background(), "sso-admin", "long-password-for-sso-admin", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	disable := func() int {
		body, _ := json.Marshal(sso.SSOSettings{Enabled: false, IssuerURL: idp.URL(), ClientID: idp.ClientID, AutoProvision: true})
		req := httptest.NewRequest(http.MethodPut, "/api/admin/sso", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		a.handleAdminSSOPut(rec, req.WithContext(context.WithValue(req.Context(), authContextKey{}, AuthContext{UserID: admin.ID, Username: admin.Username, Role: users.RoleAdmin})))
		return rec.Code
	}
	if code := disable(); code == http.StatusOK {
		t.Fatal("disabling SSO reported success while its sessions could not be ended")
	}
	if code := disable(); code != http.StatusOK {
		t.Fatal("saving again:", code)
	}
	if a.ssoStore.Load().Enabled {
		t.Fatal("SSO is still enabled")
	}
	for name, srv := range map[string]*Server{"A": a, "B": b} {
		if sessionAlive(srv, c) {
			t.Fatalf("an SSO session still works on %s after SSO was disabled", name)
		}
	}
}
