package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func probe(srv *Server, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// AC1: /livez never touches the database; /readyz follows the store and draining; both are
// no-store and HEAD matches GET.
func TestLivezAndReadyz(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/livez", "/readyz"} {
		get, head := probe(srv, http.MethodGet, path), probe(srv, http.MethodHead, path)
		if get.Code != http.StatusOK || head.Code != get.Code {
			t.Fatalf("%s: GET %d HEAD %d", path, get.Code, head.Code)
		}
		if get.Header().Get("Cache-Control") != "no-store" || head.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s is cacheable", path)
		}
	}
	if err := srv.globalStore.Close(); err != nil {
		t.Fatal(err)
	}
	if rec := probe(srv, http.MethodGet, "/livez"); rec.Code != http.StatusOK {
		t.Fatalf("/livez with the database closed: %d", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for probe(srv, http.MethodGet, "/readyz").Code != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("/readyz still ready 2 s after the database closed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var body map[string]string
	_ = json.Unmarshal(probe(srv, http.MethodGet, "/readyz").Body.Bytes(), &body)
	if len(body) != 1 || body["status"] != "unready" {
		t.Fatalf("/readyz body says more than a status word: %v", body)
	}
}

func TestReadyzStopsAnsweringReadyWhileDraining(t *testing.T) {
	srv := newTestServer(t)
	srv.StartDraining()
	if rec := probe(srv, http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining: %d", rec.Code)
	}
	if rec := probe(srv, http.MethodGet, "/livez"); rec.Code != http.StatusOK {
		t.Fatalf("/livez while draining: %d", rec.Code)
	}
}

// A hung ping reports unready within its timeout, and concurrent probes share one ping a second.
func TestReadyzPingsAtMostOncePerSecond(t *testing.T) {
	srv := newTestServer(t)
	var pings atomic.Int32
	srv.ready.ping = func(context.Context) error { pings.Add(1); return nil }
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { probe(srv, http.MethodGet, "/readyz") })
	}
	wg.Wait()
	if n := pings.Load(); n != 1 {
		t.Fatalf("50 concurrent probes cost %d pings, want 1", n)
	}
	srv.ready = readiness{ping: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	start := time.Now()
	if rec := probe(srv, http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("hung ping: %d after %v", rec.Code, time.Since(start))
	}
}

func deadlineServer(d time.Duration, h http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stall", h)
	mux.HandleFunc("/api/mail/stall", h)
	return withSecurityHeaders(withDeadline(mux, d), func() string { return "default-src 'self'" }, func() string { return "" })
}

// AC3: a handler that ignores its context, and a store call that honours it, both answer 503
// JSON at the deadline with every security header.
func TestStalledRequestAnswersAtTheDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	for name, h := range map[string]http.HandlerFunc{
		"ignores its context": func(http.ResponseWriter, *http.Request) { <-release },
		"store honours it":    func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
	} {
		rec := httptest.NewRecorder()
		start := time.Now()
		deadlineServer(200*time.Millisecond, h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stall", nil))
		if took := time.Since(start); rec.Code != http.StatusServiceUnavailable || took > time.Second {
			t.Fatalf("%s: %d after %v", name, rec.Code, took)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "request_timeout" {
			t.Fatalf("%s: body %q", name, rec.Body.String())
		}
		for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Cross-Origin-Opener-Policy"} {
			if rec.Header().Get(h) == "" {
				t.Errorf("%s: the deadline's 503 lacks %s", name, h)
			}
		}
	}
	// A long route keeps its own budget.
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		deadlineServer(100*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mail/stall", nil))
		close(done)
	}()
	<-done
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a long route was cut at the deadline: %d", rec.Code)
	}
}

// The deadline sits below the listener's WriteTimeout, so its 503 always reaches the client.
func TestRequestDeadlineFitsTheWriteTimeout(t *testing.T) {
	srv := newTestServer(t)
	srv.Prepare()
	if requestDeadline <= 0 || requestDeadline >= srv.httpServer.WriteTimeout {
		t.Fatalf("requestDeadline %v must be below WriteTimeout %v", requestDeadline, srv.httpServer.WriteTimeout)
	}
	if requestDeadline != 14*time.Second {
		t.Fatalf("requestDeadline = %v; the docs and proxy table say 14 s", requestDeadline)
	}
}

// The exemption list is exactly the docs' proxy-timeout table, and every proxy timeout there
// exceeds its route's budget.
func TestLongRoutesAreExactlyTheNamedExemptions(t *testing.T) {
	b, err := os.ReadFile("../../../docs/RUNNING.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([^`]+)` \\| ([0-9]+ (?:s|min)) \\| ([0-9]+ (?:s|min)) \\|")
	parse := func(v string) time.Duration {
		n, unit, _ := strings.Cut(v, " ")
		d, _ := time.ParseDuration(n + map[string]string{"s": "s", "min": "m"}[unit])
		return d
	}
	docs := map[string][2]time.Duration{}
	for _, m := range row.FindAllStringSubmatch(string(b), -1) {
		docs[m[1]] = [2]time.Duration{parse(m[2]), parse(m[3])}
	}
	if len(docs) != len(longRoutes) {
		t.Errorf("docs list %d long routes, the code %d", len(docs), len(longRoutes))
	}
	for _, l := range longRoutes {
		d, ok := docs[l.prefix]
		switch {
		case !ok:
			t.Errorf("%s is missing from the proxy-timeout table in docs/RUNNING.md", l.prefix)
		case d[0] != l.budget:
			t.Errorf("%s: docs budget %v, code %v", l.prefix, d[0], l.budget)
		case d[1] <= l.budget:
			t.Errorf("%s: proxy timeout %v does not exceed its budget %v", l.prefix, d[1], l.budget)
		}
		if l.reason == "" {
			t.Errorf("%s has no reason", l.prefix)
		}
	}
}

// AC2: once draining, new deposits, drills and exports answer 503 and nothing starts.
func TestDrainingRefusesNewBackupWork(t *testing.T) {
	srv := newTestServer(t)
	srv.backup, _ = backup.New(backup.Dirs{Config: srv.configDir, State: srv.stateDir, Secret: srv.configDir}, config.BackupConfig{Keep: 1}, srv.globalStore, "test")
	const password = "long-password-for-backup"
	admin, err := srv.users.Create(context.Background(), "drain-admin", password, users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(srv, admin.ID)
	srv.StartDraining()
	for _, path := range []string{"run", "drill", "export-capsule"} {
		raw, _ := json.Marshal(map[string]any{"authSecret": authSecretFor(srv, password)})
		req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/"+path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
			t.Errorf("%s while draining: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
