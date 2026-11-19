package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// readinessTTL caps database load from probes: concurrent probes share one ping a second.
const readinessTTL = time.Second

// readinessPingTimeout is how long a ping may take before the server reports unready.
const readinessPingTimeout = time.Second

// readiness caches the last store ping. The mutex is held across the ping, so a burst of
// probes waits for one result instead of starting one ping each.
type readiness struct {
	mu      sync.Mutex
	checked time.Time
	ready   bool
	ping    func(context.Context) error // the global store's Ping; tests count it
}

// StartDraining turns /readyz to 503 and refuses new backup deposits, drills and exports,
// while every other route keeps serving, so a proxy stops routing here before the listener
// closes.
func (s *Server) StartDraining() { s.draining.Store(true) }

// handleLivez says the process serves. It never touches the database or the daemon report:
// a database outage is /readyz's business, and restarting every replica would not end it.
func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz says whether to route requests here: not while draining, and not when the
// global state store does not answer a ping within readinessPingTimeout (on PostgreSQL the
// ping also fails on a read-only standby). Status word only.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.draining.Load() && s.storeReady(r.Context()) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready"})
}

func (s *Server) storeReady(ctx context.Context) bool {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if !s.ready.checked.IsZero() && time.Since(s.ready.checked) < readinessTTL {
		return s.ready.ready
	}
	// Detached from the probe so one probe hanging up does not mark the server unready. The
	// ping runs on its own goroutine: the timeout decides, not the driver's own give-up.
	ping, cancel := context.WithTimeout(context.WithoutCancel(ctx), readinessPingTimeout)
	defer cancel()
	res := make(chan error, 1)
	go func() {
		switch {
		case s.ready.ping != nil:
			res <- s.ready.ping(ping)
		case s.globalStore == nil:
			res <- errors.New("no global state store")
		default:
			res <- s.globalStore.Ping(ping)
		}
	}()
	select {
	case err := <-res:
		s.ready.ready = err == nil
	case <-ping.Done():
		s.ready.ready = false
	}
	s.ready.checked = time.Now()
	return s.ready.ready
}

// requestDeadline bounds every ordinary request. It is far below the listener's 10-minute
// WriteTimeout (sized for streamed downloads), so the 503 always reaches the client. Without
// it a frozen database holds requests open with no limit. A 503 after it does not prove the
// request had no effect: the handler may still finish once the database answers.
var requestDeadline = 14 * time.Second

// longRoute is a path prefix the deadline does not cover, with the budget its slow work
// already has. The docs' proxy-timeout table lists each with a timeout above that budget.
type longRoute struct {
	prefix string
	budget time.Duration
	reason string
}

// longRoutes are the named exemptions. Their database calls are not bounded by the deadline
// either. TestLongRoutesAreExactlyTheNamedExemptions pins this list against the docs.
var longRoutes = []longRoute{
	{"/api/admin/backup/", 16 * time.Minute, "depositBudget: snapshot, seal and upload, drill or export a capsule"},
	{"/api/export", 10 * time.Minute, "mail export streams a whole mailbox (the listener's WriteTimeout)"},
	{"/api/contacts/export", 10 * time.Minute, "address book export streams (the listener's WriteTimeout)"},
	{"/api/mail/", 10 * time.Minute, "IMAP commands (45 s each), native sends (45 s connect and TLS, 45 s SMTP, 30 s finalization), streamed attachments"},
	{"/api/inbox", 10 * time.Minute, "IMAP listing and actions (45 s per command)"},
	{"/api/rules/run", 10 * time.Minute, "applies rules over IMAP (45 s per command)"},
	{"/api/import", 10 * time.Minute, "mail import uploads and IMAP listings (2 min fetch)"},
	{"/api/imap/", 2 * time.Minute, "IMAP account test (10 s dial, 45 s per command)"},
	{"/api/contacts/carddav-client/", 2 * time.Minute, "external CardDAV sync (45 s)"},
	{"/api/contacts/import", 2 * time.Minute, "address book upload"},
	{"/api/kydrive/", 2 * time.Minute, "KyDrive upload and fetch budgets"},
	{"/api/pgp/pickup", 2 * time.Minute, "pickup upload"},
	{"/api/admin/native-recovery/", 2 * time.Minute, "recovery evidence upload and checks (1 min)"},
	{"/api/admin/mail-relay", time.Minute, "relay TLS and AUTH check, DKIM and deliverability DNS (30 s)"},
	{"/api/admin/mail-domain", time.Minute, "domain proof DNS lookups (30 s)"},
	{"/api/admin/mail/poll-now", 2 * time.Minute, "runs one poll tick over IMAP"},
	{"/mcp", 2 * time.Minute, "agent tools read and act on mail over IMAP"},
}

// longRouteFor is the exemption covering path, if any.
func longRouteFor(path string) (longRoute, bool) {
	for _, l := range longRoutes {
		if path == strings.TrimSuffix(l.prefix, "/") || strings.HasPrefix(path, l.prefix) {
			return l, true
		}
	}
	return longRoute{}, false
}

// withDeadline answers 503 JSON when next has not answered within d. Like
// http.TimeoutHandler, the handler writes into a buffer sent when it returns in time and
// discarded otherwise; its context ends at d, so store and outbound calls stop. It sits
// inside withSecurityHeaders, so the 503 carries every security header.
func withDeadline(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, long := longRouteFor(r.URL.Path); long {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		tw := &bufferedWriter{h: http.Header{}, rc: http.NewResponseController(w)}
		done, panicked := make(chan struct{}), make(chan any, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					panicked <- p
				}
			}()
			next.ServeHTTP(tw, r.WithContext(ctx))
			close(done)
		}()
		select {
		case p := <-panicked:
			panic(p)
		case <-done:
			tw.mu.Lock()
			defer tw.mu.Unlock()
			if !tw.wroteHeader {
				tw.code, tw.sent = http.StatusOK, tw.h.Clone()
			}
			for k, v := range tw.sent {
				w.Header()[k] = v
			}
			w.WriteHeader(tw.code)
			_, _ = w.Write(tw.buf.Bytes())
		case <-ctx.Done():
			tw.mu.Lock()
			defer tw.mu.Unlock()
			tw.timedOut = true
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "request_timeout",
				"error": "KyPost could not finish this request in time. It may still take effect; check before retrying."})
		}
	})
}

type bufferedWriter struct {
	// rc is the real connection's controller: an upload route still extends its read
	// deadline through it (withUploadDeadline). Nothing else reaches the connection.
	rc *http.ResponseController
	mu sync.Mutex
	h  http.Header
	// sent is h as it stood at WriteHeader: as with net/http, later changes are not sent.
	sent                  http.Header
	buf                   bytes.Buffer
	code                  int
	wroteHeader, timedOut bool
}

func (tw *bufferedWriter) Header() http.Header { return tw.h }

func (tw *bufferedWriter) SetReadDeadline(t time.Time) error { return tw.rc.SetReadDeadline(t) }

func (tw *bufferedWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if !tw.wroteHeader {
		tw.wroteHeader, tw.code, tw.sent = true, http.StatusOK, tw.h.Clone()
	}
	return tw.buf.Write(p)
}

func (tw *bufferedWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if !tw.timedOut && !tw.wroteHeader {
		tw.wroteHeader, tw.code, tw.sent = true, code, tw.h.Clone()
	}
}

// refuseWhileDraining answers 503 to new backup work once StartDraining was called.
func (s *Server) refuseWhileDraining(w http.ResponseWriter) bool {
	if !s.draining.Load() {
		return false
	}
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "draining", "error": "this replica is shutting down; try again"})
	return true
}
