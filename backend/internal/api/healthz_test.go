package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/health"
)

func TestSuiteHealthRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *health.DaemonReport
		apiOK  bool
		want   int
	}{
		{"healthy", ptrDaemonReport(health.NewDaemonReport(health.Status{Healthy: true}, time.Now())), true, http.StatusOK},
		{"missing daemon", nil, true, http.StatusServiceUnavailable},
		{"stale daemon", ptrDaemonReport(health.NewDaemonReport(health.Status{Healthy: true}, time.Now().Add(-health.DaemonHeartbeatMaxAge-time.Minute))), true, http.StatusServiceUnavailable},
		{"mailbox unavailable", ptrDaemonReport(health.NewDaemonReport(health.Status{Healthy: true}, time.Now())), false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			if tc.apiOK {
				srv.health.MarkHealthy()
			} else {
				srv.health.MarkUnhealthy("mailbox 127.0.0.1 unavailable")
			}
			if tc.report != nil {
				raw, err := tc.report.Encode()
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.globalStore.SetDaemonHealth(raw); err != nil {
					t.Fatal(err)
				}
			}
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Schema  string                                  `json:"schema"`
				Service string                                  `json:"service"`
				Status  string                                  `json:"status"`
				Checks  []struct{ Name, Status, Reason string } `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantStatus := "ok"
			if tc.want != http.StatusOK {
				wantStatus = "down"
			}
			if body.Schema != "ky.health/1" || body.Service != "kypost" || body.Status != wantStatus || len(body.Checks) != 1 || body.Checks[0].Name != "service" || body.Checks[0].Status != wantStatus || body.Checks[0].Reason != "" {
				t.Fatalf("unexpected suite health: %+v", body)
			}
			for _, secret := range []string{"mailbox", "daemon", "version", "reason", "127.0.0.1"} {
				if strings.Contains(strings.ToLower(rec.Body.String()), secret) {
					t.Fatalf("response exposes %q: %s", secret, rec.Body.String())
				}
			}
		})
	}
}

func ptrDaemonReport(report health.DaemonReport) *health.DaemonReport { return &report }

func TestSuiteHealthCachesResult(t *testing.T) {
	srv := newTestServer(t)
	srv.health.MarkHealthy()
	raw, err := health.NewDaemonReport(health.Status{Healthy: true}, time.Now()).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.globalStore.SetDaemonHealth(raw); err != nil {
		t.Fatal(err)
	}
	h := srv.routes()
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec
	}
	first := request()
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	if err := srv.globalStore.SetDaemonHealth(""); err != nil {
		t.Fatal(err)
	}
	second := request()
	if second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("cache miss: first=%s second=%s", first.Body.String(), second.Body.String())
	}
}
