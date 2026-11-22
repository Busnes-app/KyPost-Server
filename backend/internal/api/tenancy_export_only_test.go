package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestExportOnlySession: during deletion_scheduled an export_only token signs in to an
// export-only session (K open question 4 (a)). It reaches the export routes and nothing
// else, every other route answering as for no session; it ends with the org's window.
func TestExportOnlySession(t *testing.T) {
	srv, idp, db := multiSSOServer(t)
	provisionMember(t, srv, db, "alice", orgA)
	provisionMember(t, srv, db, "bob", orgB)
	exportClaims := func(sub, org string) map[string]any {
		return map[string]any{"sub": sub, "preferred_username": "u_" + sub, "roles": []string{}, "org_id": org, "export_only": true}
	}
	// While the org is active an export-only token gets nothing.
	if rec := signInRecorder(t, srv, idp, exportClaims("alice", orgA)); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatal("export-only sign-in for an active org", rec.Code)
	}
	setOrg(t, db, orgA, "deletion_scheduled", true, `{}`)
	// An ordinary token gets nothing while deletion is scheduled.
	if rec := signInRecorder(t, srv, idp, map[string]any{"sub": "alice", "preferred_username": "u_alice", "roles": []string{}, "org_id": orgA}); rec.Code != http.StatusForbidden {
		t.Fatal("ordinary sign-in during deletion_scheduled", rec.Code)
	}
	// Another org's export-only token is refused like a wrong issuer.
	if rec := signInRecorder(t, srv, idp, exportClaims("bob", orgA)); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatal("export-only token for another org's member", rec.Code)
	}
	rec := signInRecorder(t, srv, idp, exportClaims("alice", orgA))
	cookie := sessionCookieFrom(rec)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != exportOnlyLanding || cookie == nil {
		t.Fatalf("export-only sign-in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	call := func(method, path string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	me := call(http.MethodGet, "/api/auth/me", cookie)
	var got struct {
		ExportOnly bool   `json:"exportOnly"`
		Role       string `json:"role"`
	}
	if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &got) != nil || !got.ExportOnly || got.Role != "user" {
		t.Fatalf("/api/auth/me: %d %s", me.Code, me.Body)
	}
	if w := call(http.MethodGet, "/api/mailboxes", cookie); w.Code == http.StatusUnauthorized {
		t.Fatal("the mailbox list refused an export-only session")
	}
	// Every other route answers exactly as with no session, and the session survives.
	param := regexp.MustCompile(`\{[^}]*\}`)
	for _, r := range crossOrgRoutes(t) {
		if exportOnlyRoutes[r.method+" "+r.pattern] {
			continue
		}
		path := param.ReplaceAllString(r.pattern, "x")
		with, without := call(r.method, path, cookie), call(r.method, path, nil)
		if with.Code != without.Code {
			t.Errorf("%s %s: export-only session %d, no session %d", r.method, r.pattern, with.Code, without.Code)
		}
	}
	if w := call(http.MethodGet, "/api/auth/me", cookie); !strings.Contains(w.Body.String(), `"exportOnly":true`) {
		t.Fatal("a refused route ended the export-only session", w.Body)
	}
	// The window ends with deletion: the session is refused.
	setOrg(t, db, orgA, "deleted", false, `{}`)
	if w := call(http.MethodGet, "/api/mailboxes", cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("an export-only session outlived its org's deletion", w.Code)
	}
}

// TestSingleTenantIgnoresExportOnly: a single install reads no export_only claim, so a token
// carrying one signs in as before.
func TestSingleTenantIgnoresExportOnly(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	idp.SetClaims(map[string]any{"sub": "solo", "preferred_username": "solo", "export_only": true})
	rec := runSSOFlow(t, srv, idp, nil, false)
	if rec.Code == http.StatusFound && rec.Header().Get("Location") == exportOnlyLanding {
		t.Fatal("single-tenant made an export-only session")
	}
}
