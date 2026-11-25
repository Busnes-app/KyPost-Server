package api

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

// Proof-of-work escalation prices an address alike on every table-mode replica: failures
// counted on one raise the other's difficulty, a success forgives only its own account, and
// a database that cannot answer prices the address at the ceiling. A single pair keeps it
// in memory.
func TestTablePoWEscalationIsSharedAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	plainA, plainB := sharedPair(t)
	if plainA.powDifficulty.shared != nil || plainB.powDifficulty.shared != nil {
		t.Fatal("a single install shared the proof-of-work escalation")
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
	pa, pb := a.powDifficulty, b.powDifficulty
	const base, ip = 1000, "198.51.100.9"
	now := time.Now()
	pa.recordFailure(ip, "alice", now)
	pb.recordFailure(ip, "bob", now)
	pa.recordFailure(ip, "bob", now)
	want := base * powEscalationFactor * powEscalationFactor * powEscalationFactor
	for _, p := range []*powEscalation{pa, pb} {
		if got := p.maxNumberFor(ip, base, now); got != want {
			t.Fatalf("difficulty %d, want %d from three failures across replicas", got, want)
		}
	}
	pb.clearAccount(ip, "bob")
	if got := pa.maxNumberFor(ip, base, now); got != base*powEscalationFactor {
		t.Fatalf("after bob signed in on B: %d, want alice's failure to still count", got)
	}
	if got := pa.maxNumberFor("203.0.113.1", base, now); got != base {
		t.Fatalf("another address: %d, want base", got)
	}
	down, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = down.Close()
	pb.shared = down
	if got := pb.maxNumberFor("203.0.113.1", base, now); got != powMaxNumberCeiling {
		t.Fatalf("a failed database priced %d, want the ceiling", got)
	}
}
