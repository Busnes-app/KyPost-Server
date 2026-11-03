package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// index.html names the current hashed bundles, so browsers must revalidate it: a tab opened
// after an upgrade then loads the new build. Hashed assets keep their year-long caching.
func TestFrontendEntryDocumentRevalidatesAndAssetsKeepCaching(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": `<script type="module" src="/assets/index-abc.js"></script>`, "assets/index-abc.js": "export {}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FRONTEND_DIR", dir)
	s := &Server{}
	for path, want := range map[string]string{
		"/":                    "no-cache",
		"/read/inbox":          "no-cache",
		"/index.html":          "no-cache",
		"/assets/index-abc.js": "public, max-age=31536000, immutable",
	} {
		rec := httptest.NewRecorder()
		s.handleFrontend(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: status %d, Cache-Control %q, want %q", path, rec.Code, got, want)
		}
	}
}
