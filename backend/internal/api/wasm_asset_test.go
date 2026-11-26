package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/captcha"
)

func frontendFixture(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FRONTEND_DIR", dir)
}

func scriptSources(policy string) []string {
	for _, d := range strings.Split(policy, "; ") {
		if rest, ok := strings.CutPrefix(d, "script-src "); ok {
			return strings.Fields(rest)
		}
	}
	return nil
}

// The browser's OpenPGP engine is WebAssembly, so the app shell document may
// compile wasm. Nothing else gets that source, and nothing gets 'unsafe-eval'.
func TestCSPAllowsWasmOnlyForTheAppShell(t *testing.T) {
	frontendFixture(t, map[string]string{"index.html": "<!doctype html>", "assets/a.js": "export {}", "assets/e.wasm": "\x00asm"})
	for _, provider := range []captcha.Provider{"", captcha.ProviderFriendly} {
		s := &Server{captchaProvider: provider}
		// The real chain: withDeadline hands the handler a fresh header map.
		h := withSecurityHeaders(withDeadline(http.HandlerFunc(s.handleFrontend), time.Minute), s.contentSecurityPolicy, func() string { return "" })
		for path, wantWasm := range map[string]bool{"/": true, "/index.html": true, "/read/inbox": true,
			"/assets/a.js": provider == captcha.ProviderFriendly, "/assets/e.wasm": provider == captcha.ProviderFriendly, "/api/nope": provider == captcha.ProviderFriendly} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			src := scriptSources(rec.Header().Get("Content-Security-Policy"))
			n := 0
			for _, s := range src {
				if s == "'wasm-unsafe-eval'" {
					n++
				}
			}
			if (n == 1) != wantWasm || n > 1 || slices.Contains(src, "'unsafe-eval'") || slices.Contains(src, "'unsafe-inline'") {
				t.Errorf("provider %q %s: script-src %v, want wasm=%v exactly once and never unsafe-eval", provider, path, src, wantWasm)
			}
		}
	}
}

// The ~9 MB module ships with .br and .gz siblings; static files are
// otherwise sent uncompressed.
func TestWasmIsServedPrecompressed(t *testing.T) {
	frontendFixture(t, map[string]string{"assets/e.wasm": "raw", "assets/e.wasm.br": "brotli", "assets/e.wasm.gz": "gzip"})
	s := &Server{}
	for accept, want := range map[string][2]string{
		"gzip, deflate, br": {"br", "brotli"},
		"gzip":              {"gzip", "gzip"},
		"br;q=0, gzip":      {"gzip", "gzip"},
		"":                  {"", "raw"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/assets/e.wasm", nil)
		req.Header.Set("Accept-Encoding", accept)
		rec := httptest.NewRecorder()
		s.handleFrontend(rec, req)
		h := rec.Header()
		if rec.Code != http.StatusOK || h.Get("Content-Type") != "application/wasm" || h.Get("Content-Encoding") != want[0] ||
			rec.Body.String() != want[1] || !strings.Contains(strings.Join(h.Values("Vary"), ","), "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q: %d %v body %q, want encoding %q body %q", accept, rec.Code, h, rec.Body, want[0], want[1])
		}
	}
	// No siblings: the raw module, still typed for instantiateStreaming.
	frontendFixture(t, map[string]string{"assets/e.wasm": "raw"})
	req := httptest.NewRequest(http.MethodGet, "/assets/e.wasm", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	s.handleFrontend(rec, req)
	if rec.Body.String() != "raw" || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != "application/wasm" {
		t.Errorf("without siblings: %v %q", rec.Header(), rec.Body)
	}
}
