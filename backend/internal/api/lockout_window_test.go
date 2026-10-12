package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// fakeLockoutClock pins l's clock and returns a function that advances it.
func fakeLockoutClock(l *failureLockout) (advance func(time.Duration)) {
	now := time.Now()
	l.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

// The IP window is measured from failures. Successful traffic from the same
// address must neither keep old failures alive past it nor forgive failures
// still inside it.
func TestDAVLockoutWindowRunsFromFailures(t *testing.T) {
	srv := newTestServer(t)
	all, err := srv.users.List()
	if err != nil || len(all) == 0 {
		t.Fatalf("no test user available: %v", err)
	}
	u := all[0]
	hash, err := users.HashPassword(context.Background(), "dav-app-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := srv.writeDAVPassword(u.ID, davPasswordFile{Hash: hash, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("writeDAVPassword: %v", err)
	}
	const maxFailures = 3
	srv.davLockout = newFailureLockout(maxFailures, davLockoutFor)
	advance := fakeLockoutClock(srv.davLockout)

	const ip = "203.0.113.40:40000"
	fail := func(want int) {
		t.Helper()
		if rec := davRequest(srv, u.Username, "wrong", ip); rec.Code != want {
			t.Fatalf("failed attempt: status = %d, want %d", rec.Code, want)
		}
	}
	poll := func() {
		t.Helper()
		if rec := davRequest(srv, u.Username, "dav-app-password", ip); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusTooManyRequests {
			t.Fatalf("polling with the correct password: status = %d", rec.Code)
		}
	}

	// Expiry: failures followed by 20 minutes of polling are forgotten.
	for range maxFailures - 1 {
		fail(http.StatusUnauthorized)
	}
	for range 4 {
		advance(5 * time.Minute)
		poll()
	}
	for range maxFailures {
		fail(http.StatusUnauthorized)
	}
	fail(http.StatusTooManyRequests)

	// Accumulation: failures 14 minutes apart, polled between, still lock.
	advance(davLockoutFor)
	for range maxFailures - 1 {
		fail(http.StatusUnauthorized)
		advance(7 * time.Minute)
		poll()
		advance(7 * time.Minute)
	}
	fail(http.StatusUnauthorized)
	fail(http.StatusTooManyRequests)
}

// Same contract on the login per-IP budget.
func TestLoginIPLockoutWindowRunsFromFailures(t *testing.T) {
	srv := newTestServer(t)
	srv.loginRateLimiter = nil
	const maxFailures = 3
	srv.loginIPLockout = newFailureLockout(maxFailures, loginIPLockoutFor)
	srv.loginLockout = newFailureLockout(100, loginIPLockoutFor)
	advance := fakeLockoutClock(srv.loginIPLockout)
	for _, name := range []string{"window-a", "window-b"} {
		if _, err := srv.users.Create(context.Background(), name, "correct-horse-battery-staple", users.RoleUser); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	login := func(username, password string, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.RemoteAddr = "198.51.100.91:4000"
		rec := httptest.NewRecorder()
		srv.handleLogin(rec, req)
		if rec.Code != want {
			t.Fatalf("login %s: status = %d, want %d", username, rec.Code, want)
		}
	}
	fail := func(want int) { t.Helper(); login("window-b", "wrong-password-entirely", want) }
	succeed := func() { t.Helper(); login("window-a", "correct-horse-battery-staple", http.StatusOK) }

	for range maxFailures - 1 {
		fail(http.StatusUnauthorized)
	}
	for range 4 {
		advance(5 * time.Minute)
		succeed()
	}
	for range maxFailures {
		fail(http.StatusUnauthorized)
	}
	fail(http.StatusTooManyRequests)

	advance(loginIPLockoutFor)
	for range maxFailures - 1 {
		fail(http.StatusUnauthorized)
		advance(7 * time.Minute)
		succeed()
		advance(7 * time.Minute)
	}
	fail(http.StatusUnauthorized)
	fail(http.StatusTooManyRequests)
}
