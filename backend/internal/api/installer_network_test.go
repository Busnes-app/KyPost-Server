package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInstallerNetworkPublicSpoofBoundary(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("SERVER_BASE_URL", "")
	for _, trusted := range []bool{false, true} {
		cidrs := ""
		if trusted {
			cidrs = "127.0.0.1/32"
		}
		trustProxyCIDRsForTest(t, cidrs)
		r := httptest.NewRequest(http.MethodGet, "/api/installer/network", nil)
		r.RemoteAddr = "127.0.0.1:42000"
		r.Header.Set("X-Forwarded-For", "198.51.100.21")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out) != 3 {
			t.Fatalf("unsafe public report: %d %s", w.Code, w.Body)
		}
		ip, scheme := "127.0.0.1", "http"
		if trusted {
			ip, scheme = "198.51.100.21", "https"
		}
		if out["clientIp"] != ip || out["scheme"] != scheme || out["proxyHeadersTrusted"] != trusted {
			t.Fatal(out)
		}
	}
}
