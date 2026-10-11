package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// retiredScryptHash has the shape of a pre-Argon2id hash at the last
// production cost. Nothing verifies it any more.
const retiredScryptHash = "scrypt$131072$8$1$c2FsdHNhbHRzYWx0c2FsdA==$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g="

// plantPasswordHash writes hash straight into users.json, the way an account
// written by an older install looks. There is no exported Store method for
// writing an arbitrary hash, on purpose.
func plantPasswordHash(t *testing.T, srv *Server, userID, hash string) {
	t.Helper()
	usersPath := filepath.Join(srv.configDir, "users.json")
	raw, err := os.ReadFile(usersPath)
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	var file struct {
		Version int              `json:"version"`
		Users   []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("unmarshal users.json: %v", err)
	}
	planted := false
	for _, entry := range file.Users {
		if entry["id"] == userID {
			entry["passwordHash"] = hash
			planted = true
		}
	}
	if !planted {
		t.Fatalf("user %q not found in users.json", userID)
	}
	out, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal users.json: %v", err)
	}
	if err := os.WriteFile(usersPath, out, 0o600); err != nil {
		t.Fatalf("write users.json: %v", err)
	}
	stored, err := srv.users.Get(userID)
	if err != nil {
		t.Fatalf("Get after planting: %v", err)
	}
	if stored.PasswordHash != hash {
		t.Fatal("planted hash did not stick")
	}
}

// TestRetiredScryptAccountFailsLikeAWrongPassword: the right password against a
// pre-Argon2id hash is answered exactly as a wrong one, and the hash is left
// for an admin to reset rather than rewritten.
func TestRetiredScryptAccountFailsLikeAWrongPassword(t *testing.T) {
	srv := newTestServer(t)
	const pw = "correct-horse-battery-staple"
	u, err := srv.users.Create(context.Background(), "retired-scrypt", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantPasswordHash(t, srv, u.ID, retiredScryptHash)

	right := loginAttempt(srv, u.Username, pw, "203.0.113.50:40000")
	wrong := loginAttempt(srv, u.Username, "wrong-password", "203.0.113.51:40000")
	if right.Code != http.StatusUnauthorized || right.Code != wrong.Code || right.Body.String() != wrong.Body.String() {
		t.Fatalf("right password: %d %q; wrong password: %d %q — want identical 401s",
			right.Code, right.Body.String(), wrong.Code, wrong.Body.String())
	}
	if stored, _ := srv.users.Get(u.ID); stored.PasswordHash != retiredScryptHash {
		t.Fatal("login rewrote a retired hash; only an admin reset may replace it")
	}
}

// timingSamples is how many attempts each case makes. The MINIMUM is kept:
// that is the figure an attacker gets by repeating, and it is the statistic
// least polluted by a scheduler hiccup.
const timingSamples = 3

// fastest runs attempt timingSamples times and returns the shortest.
// Each call gets its own source address so the per-(username, IP) lockout
// (three strikes) never fires and never becomes the thing being measured.
func fastest(t *testing.T, name string, attempt func(remoteAddr string)) time.Duration {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for i := range timingSamples {
		addr := fmt.Sprintf("203.0.113.%d:40000", 100+i)
		start := time.Now()
		attempt(addr)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	t.Logf("%s: %v", name, best)
	return best
}

// assertTimingSpread fails when the slowest case is more than a small multiple
// of the fastest. A RATIO rather than an absolute band: an absolute millisecond
// figure would be a statement about the CI runner instead of about the code.
func assertTimingSpread(t *testing.T, timings map[string]time.Duration) {
	t.Helper()
	const maxRatio = 1.5
	var slowest, quickest time.Duration
	var slowestName, quickestName string
	for name, d := range timings {
		if slowest == 0 || d > slowest {
			slowest, slowestName = d, name
		}
		if quickest == 0 || d < quickest {
			quickest, quickestName = d, name
		}
	}
	if ratio := float64(slowest) / float64(quickest); ratio > maxRatio {
		t.Fatalf("credential-check timing spread %.2fx (%s %v vs %s %v), want under %.1fx — "+
			"response timing tells an anonymous caller which accounts exist",
			ratio, slowestName, slowest, quickestName, quickest, maxRatio)
	}
}

// TestLoginTimingDoesNotRevealUnknownUsernames is the account-enumeration
// regression. It plants a RETIRED hash on purpose: an account that never
// verifies must still cost one Argon2id verify (users' dummy), or it answers
// instantly and marks exactly the dormant accounts an attacker wants to find.
// Production cost, so the ratio measures the KDF rather than scheduler noise.
func TestLoginTimingDoesNotRevealUnknownUsernames(t *testing.T) {
	withProductionHashCost(t)
	srv := newTestServer(t)
	const pw = "correct-horse-battery-staple"
	modern, err := srv.users.Create(context.Background(), "timing-argon2id", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	legacy, err := srv.users.Create(context.Background(), "timing-legacy", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantPasswordHash(t, srv, legacy.ID, retiredScryptHash)
	older, err := srv.users.Create(context.Background(), "timing-older-cost", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantPasswordHash(t, srv, older.ID, olderCostArgon2Hash(t, pw))

	wrongPassword := func(username string) func(string) {
		return func(addr string) {
			if rec := loginAttempt(srv, username, "wrong-password", addr); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", username, rec.Code)
			}
		}
	}
	assertTimingSpread(t, map[string]time.Duration{
		"argon2id account":       fastest(t, "argon2id account", wrongPassword(modern.Username)),
		"retired scrypt account": fastest(t, "retired scrypt account", wrongPassword(legacy.Username)),
		"older-cost argon2id":    fastest(t, "older-cost argon2id", wrongPassword(older.Username)),
		"unknown username":       fastest(t, "unknown username", wrongPassword("no-such-user-anywhere")),
	})
}

// olderCostArgon2Hash mints a still-supported Argon2id hash at the library's
// minimum cost — what an account written before a cost raise holds until it
// next signs in.
func olderCostArgon2Hash(t *testing.T, secret string) string {
	t.Helper()
	h, err := password.HashWith(secret, password.Params{Memory: 8 * 1024, Time: 1, Threads: 1})
	if err != nil {
		t.Fatalf("HashWith: %v", err)
	}
	if !users.NeedsRehash(h) {
		t.Fatal("fixture is not below the current cost")
	}
	return h
}

// TestReviewPairingTimingDoesNotRevealTheConfiguredAccount is the third
// unauthenticated credential check in the tree. It is bounded — the endpoint
// 404s unless an operator sets REVIEW_PAIRING_USERNAME — but on an install that
// does set it, a cheaper miss says which name was configured.
func TestReviewPairingTimingDoesNotRevealTheConfiguredAccount(t *testing.T) {
	withProductionHashCost(t)
	t.Setenv("REVIEW_PAIRING_USERNAME", "review-*")
	srv := newTestServer(t)
	const pw = "correct-horse-battery-staple"
	modern, err := srv.users.Create(context.Background(), "review-argon2id", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	legacy, err := srv.users.Create(context.Background(), "review-legacy", pw, users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantPasswordHash(t, srv, legacy.ID, retiredScryptHash)

	wrongPassword := func(username string) func(string) {
		return func(addr string) {
			body, _ := json.Marshal(map[string]string{"username": username, "password": "wrong-password"})
			req := httptest.NewRequest(http.MethodPost, "/api/notifications/review-pairing", bytes.NewReader(body))
			req.RemoteAddr = addr
			rec := httptest.NewRecorder()
			srv.handleReviewPairing(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", username, rec.Code)
			}
		}
	}
	assertTimingSpread(t, map[string]time.Duration{
		"argon2id review account": fastest(t, "argon2id review account", wrongPassword(modern.Username)),
		"retired review account":  fastest(t, "retired review account", wrongPassword(legacy.Username)),
		"unmatched username":      fastest(t, "unmatched username", wrongPassword("no-such-user-anywhere")),
	})
}

// TestDAVAuthTimingDoesNotRevealUsernames is the same oracle on the CardDAV
// surface. A wrong password against an account whose app-password file still
// holds a retired scrypt hash must not be distinguishable from an unknown
// username.
func TestDAVAuthTimingDoesNotRevealUsernames(t *testing.T) {
	withProductionHashCost(t)
	srv := newTestServer(t)

	modern, err := srv.users.Create(context.Background(), "dav-timing-argon2id", "pw-dav-timing-argon2id-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	modernHash, err := users.HashPassword(context.Background(), "app-password-modern")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := srv.writeDAVPassword(modern.ID, davPasswordFile{Hash: modernHash}); err != nil {
		t.Fatalf("writeDAVPassword: %v", err)
	}

	legacy, err := srv.users.Create(context.Background(), "dav-timing-legacy", "pw-dav-timing-legacy-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := srv.writeDAVPassword(legacy.ID, davPasswordFile{Hash: retiredScryptHash}); err != nil {
		t.Fatalf("writeDAVPassword: %v", err)
	}
	older, err := srv.users.Create(context.Background(), "dav-timing-older", "pw-dav-timing-older-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := srv.writeDAVPassword(older.ID, davPasswordFile{Hash: olderCostArgon2Hash(t, "app-password-older")}); err != nil {
		t.Fatalf("writeDAVPassword: %v", err)
	}

	handler := srv.withDAVBasicAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a wrong app password reached the handler")
	}))
	wrongPassword := func(username string) func(string) {
		return func(addr string) {
			req := httptest.NewRequest("PROPFIND", davPrefix+"/"+username+"/", nil)
			req.SetBasicAuth(username, "not-the-app-password")
			req.RemoteAddr = addr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", username, rec.Code)
			}
		}
	}
	assertTimingSpread(t, map[string]time.Duration{
		"argon2id app password":   fastest(t, "argon2id app password", wrongPassword(modern.Username)),
		"retired app password":    fastest(t, "retired app password", wrongPassword(legacy.Username)),
		"older-cost app password": fastest(t, "older-cost app password", wrongPassword(older.Username)),
		"unknown username":        fastest(t, "unknown username", wrongPassword("no-such-user-anywhere")),
		"no app password":         fastest(t, "no app password", wrongPassword("admin")),
	})
}

// TestSettleCredentialTimingStopsWhenTheClientLeaves: the floor must not hold a
// handler goroutine for a caller that has gone away.
func TestSettleCredentialTimingStopsWhenTheClientLeaves(t *testing.T) {
	withProductionHashCost(t)
	srv := newTestServer(t)
	floor, err := users.DummyVerifyCost(context.Background())
	if err != nil {
		t.Fatalf("DummyVerifyCost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	srv.settleCredentialTiming(ctx, start)
	if elapsed := time.Since(start); elapsed >= floor/2 {
		t.Fatalf("settled for %v after cancellation; floor is %v", elapsed, floor)
	}
}
