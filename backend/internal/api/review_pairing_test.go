package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func reviewPairing(srv *Server, body map[string]string, addr string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/review-pairing", bytes.NewReader(raw))
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	srv.handleReviewPairing(rec, req)
	return rec
}

func TestReviewPairingRequiresOptInAndConfiguredAccountCredential(t *testing.T) {
	srv := newTestServer(t)
	const pw = "review-password-123"
	if rec := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": strings.Repeat("0", 64)}, "203.0.113.70:1"); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want 404", rec.Code)
	}

	u, err := srv.users.Create(context.Background(), "play-review", pw, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.ClearMustChangePassword(u.ID); err != nil {
		t.Fatal(err)
	}
	other, err := srv.users.Create(context.Background(), "play-review2", pw, users.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.ClearMustChangePassword(other.ID); err != nil {
		t.Fatal(err)
	}
	srv.serverBaseURL = "http://review.example"

	// A trailing * is no longer a wildcard: it names no account at all.
	t.Setenv("REVIEW_PAIRING_USERNAME", "play-*")
	wildcard := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": testAuthSecret(srv, "play-review", pw)}, "203.0.113.71:1")
	if wildcard.Code != http.StatusUnauthorized {
		t.Fatalf("wildcard status = %d, want 401", wildcard.Code)
	}

	t.Setenv("REVIEW_PAIRING_USERNAME", "Play-Review")
	wrong := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": strings.Repeat("0", 64)}, "203.0.113.72:1")
	for name, body := range map[string]map[string]string{
		"another valid account": {"username": "play-review2", "authSecret": testAuthSecret(srv, "play-review2", pw)},
		"plaintext password":    {"username": "play-review", "password": pw},
	} {
		rec := reviewPairing(srv, body, "203.0.113.73:1")
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != wrong.Body.String() {
			t.Fatalf("%s: %d %q, want the wrong-credential answer %q", name, rec.Code, rec.Body.String(), wrong.Body.String())
		}
	}

	ok := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": testAuthSecret(srv, "play-review", pw)}, "203.0.113.74:1")
	if ok.Code != http.StatusOK {
		t.Fatalf("valid status = %d: %s", ok.Code, ok.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(ok.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body["deepLink"].(string), "kypost://native-pair?") {
		t.Fatalf("deepLink = %q", body["deepLink"])
	}
}

// TestReviewPairingSharesLoginProtections: review pairing spends the same
// per-IP lockout and refuses a held restore, like handleLogin.
func TestReviewPairingSharesLoginProtections(t *testing.T) {
	t.Setenv("REVIEW_PAIRING_USERNAME", "play-review")
	srv := newTestServer(t)
	lockOutAddress(t, srv, "203.0.113.80")
	rec := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": strings.Repeat("0", 64)}, "203.0.113.80:1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out address: status %d, want 429", rec.Code)
	}
}

// Review pairing's per-account strikes go to its own table, never loginLockout,
// so the route cannot fill the table every sign-in shares.
func TestReviewPairingKeepsItsOwnAccountLockout(t *testing.T) {
	t.Setenv("REVIEW_PAIRING_USERNAME", "play-review")
	srv := newTestServer(t)
	rec := reviewPairing(srv, map[string]string{"username": "play-review", "authSecret": strings.Repeat("0", 64)}, "203.0.113.81:1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credential: status %d, want 401", rec.Code)
	}
	for l, want := range map[*failureLockout]int{srv.loginLockout: 0, srv.reviewPairingLockout: 1} {
		l.mu.Lock()
		n := len(l.entries)
		l.mu.Unlock()
		if n != want {
			t.Errorf("lockout holds %d entries, want %d", n, want)
		}
	}
}
