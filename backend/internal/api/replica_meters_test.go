package api

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// tableMeters is every MH6 meter of s, by name.
func tableMeters(s *Server) map[string]*keyedLimiter {
	return map[string]*keyedLimiter{
		"mfa-push": s.mfaPushLimiter, "send-as": s.sendAsCooldown, "kypulse-confirm": s.kypulseConfirmCooldown,
		"notification-test": s.notificationTestCooldown, "native-relay-check": s.nativeRelayCheckCooldown,
		"deliverability": s.deliverabilityCooldown, "pow-challenges": s.powChallenges, "login-params": s.loginParamsLimiter,
	}
}

// MH6: in table mode each meter gives one total across two replicas, and a database that
// cannot answer refuses rather than falling back to memory. Single mode keeps them per replica.
func TestTableMetersAreSharedAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	plainA, plainB := sharedPair(t)
	for name, l := range tableMeters(plainA) {
		if l.shared != nil || tableMeters(plainB)[name].shared != nil {
			t.Fatal("a single install shared", name)
		}
	}

	a := newTestServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	mountServerDocuments(t, a)
	b := replicaOf(t, a)
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	bMeters := tableMeters(b)
	for name, la := range tableMeters(a) {
		lb := bMeters[name]
		limit, _ := la.sharedWindow(1)
		for i := range limit {
			l := []*keyedLimiter{la, lb}[i%2]
			if ok, _ := l.allow("key-" + name); !ok {
				t.Fatalf("%s refused hit %d of %d", name, i+1, limit)
			}
		}
		for _, l := range []*keyedLimiter{la, lb} {
			if ok, wait := l.allow("key-" + name); ok || wait <= 0 {
				t.Fatalf("%s admitted more than %d across two replicas", name, limit)
			}
		}
		if ok, _ := lb.allow("other-" + name); !ok {
			t.Fatal("another key shared a window", name)
		}
	}
	down, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = down.Close()
	for name, l := range tableMeters(b) {
		l.shared = down
		if ok, wait := l.allow("fresh-" + name); ok || wait <= 0 {
			t.Fatal("a failed database admitted", name)
		}
	}
}

// A shared login-params window still refunds every accepted back-channel logout, so a burst
// of real logouts across two table-mode replicas is never throttled.
func TestTableBackchannelLogoutsAreRefundedAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, idp := setupSSOTestServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	mountServerDocuments(t, a)
	b := replicaOf(t, a)
	b.serverBaseURL = a.serverBaseURL
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	if a.loginParamsLimiter.shared == nil {
		t.Fatal("login-params is not shared in table mode")
	}
	for i := range loginParamsBurst + 5 {
		rec := postLogout([]*Server{a, b}[i%2], logoutForm(idp.LogoutToken(t, "alice", "sid-"+strconv.Itoa(i), nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200: %s", i, rec.Code, rec.Body.String())
		}
	}
}
