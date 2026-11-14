package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// peopleIdP answers /api/suite/people for KyPost's client credentials and records each query.
type peopleIdP struct {
	*httptest.Server
	mu    sync.Mutex
	asked []url.Values
	fail  bool
}

func newPeopleIdP(t *testing.T) *peopleIdP {
	idp := &peopleIdP{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); r.URL.Path != "/api/suite/people" || !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		idp.mu.Lock()
		idp.asked = append(idp.asked, r.URL.Query())
		fail := idp.fail
		idp.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"people":[{"sub":"s-1","name":"Alice <b>Example</b>","email":"alice@example.com"},{"sub":"","name":"No Sub","email":"x@example.com"}]}`)
	}))
	t.Cleanup(idp.Close)
	return idp
}

func people(t *testing.T, s *Server, u users.User, q string) (int, string) {
	t.Helper()
	rec := requestAs(s, u.ID, "GET", "/api/suite/people?q="+url.QueryEscape(q))
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("not no-store")
	}
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestSuitePeople(t *testing.T) {
	idp := newPeopleIdP(t)
	s := newTestServer(t)
	if err := s.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL + "/", ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	ann := ssoUser(t, s, "ann", idp.URL, "sub-ann")
	want := `{"people":[{"name":"Alice \u003cb\u003eExample\u003c/b\u003e","email":"alice@example.com"}]}`
	if code, body := people(t, s, ann, "Ali"); code != 200 || body != want {
		t.Fatalf("people: %d %q want %q", code, body, want)
	}
	if q := idp.asked[0]; q.Get("sub") != "sub-ann" || q.Get("q") != "Ali" || q.Get("limit") != "10" {
		t.Fatalf("KyIdentity asked %v", q)
	}
	// Cached per user and query for a minute (case-insensitive).
	people(t, s, ann, "ali")
	if len(idp.asked) != 1 {
		t.Fatalf("KyIdentity asked %d times", len(idp.asked))
	}
	// Too short or too long, unlinked accounts and failures: an empty list, KyIdentity not asked.
	local := ssoUser(t, s, "loc", "", "")
	other := ssoUser(t, s, "oth", "https://other.example.com", "sub-oth")
	for name, tc := range map[string]struct {
		u users.User
		q string
	}{"short": {ann, "a"}, "long": {ann, strings.Repeat("a", 65)}, "local": {local, "ali"}, "other issuer": {other, "ali"}} {
		if code, body := people(t, s, tc.u, tc.q); code != 200 || body != `{"people":[]}` {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(idp.asked) != 1 {
		t.Fatalf("KyIdentity asked for a refused query: %d", len(idp.asked))
	}
	idp.mu.Lock()
	idp.fail = true
	idp.mu.Unlock()
	if code, body := people(t, s, ann, "bob"); code != 200 || body != `{"people":[]}` {
		t.Fatalf("failure: %d %s", code, body)
	}
	// Rate limited per user.
	s.peopleLimiter = newKeyedLimiter(1, 0)
	people(t, s, ann, "carol")
	rec := requestAs(s, ann.ID, "GET", "/api/suite/people?q=dave")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 429 || out["code"] != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %s", rec.Code, rec.Body)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/suite/people?q=ali", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
}
