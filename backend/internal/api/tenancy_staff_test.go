//go:build linux

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The staff console's tests (tenancy design "Admin surface", build plan PR 4): the cross-org
// matrix's staff variant, every administrator route as a customer member, X-Ky-Org from a
// customer, an unknown org and a malformed value, the K6 refusals, and single mode.

const staffToken = "staff-session-token"

// staffFixture is newOrgFixture plus a staff administrator, an audit log, and orgs A and B
// in the entitlement cache.
func staffFixture(t *testing.T) (*orgFixture, *syncBuffer, users.User) {
	t.Helper()
	fx, logs := newOrgFixture(t)
	withAuditLog(t, fx.srv)
	for _, org := range []string{orgA, orgB} {
		if _, err := fx.srv.tenancy.Exec(`INSERT INTO orgs(id, status, revision, enabled, quotas_json, polled_at) VALUES(?, 'active', 1, 1, '{}', 0)`, org); err != nil {
			t.Fatal(err)
		}
	}
	directoryStatus(t, postDirectory(t, fx.srv, testSyncKey, "user.created", "ev-staff", 1, orgUser("staff-1", staffOrg, true, "kypost.admin")))
	staff, err := fx.srv.users.GetBySSOSub("staff-1")
	if err != nil || staff.Role != users.RoleAdmin {
		t.Fatalf("staff: %+v %v", staff, err)
	}
	if _, err := fx.srv.users.ClearMustChangePassword(staff.ID); err != nil {
		t.Fatal(err)
	}
	return fx, logs, staff
}

// staffCall sends one request as the staff administrator on a KySignOn session, with org in
// X-Ky-Org when it is set. A write carries a verified step-up grant for its exact action.
func (fx *orgFixture) staffCall(staff users.User, method, path, body, org string) *httptest.ResponseRecorder {
	fx.t.Helper()
	sess := Session{UserID: staff.ID, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), CSRFToken: "staff-csrf", SSOKySignOn: true}
	if fx.srv.shared != nil {
		ctx := context.Background()
		_ = fx.srv.dropSession(ctx, staffToken)
		check(fx, fx.srv.putSharedSession(ctx, staffToken, sess))
	} else {
		fx.srv.sessMu.Lock()
		fx.srv.sessions[staffToken] = sess
		fx.srv.sessMu.Unlock()
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "kypost_session", Value: staffToken})
	req.Header.Set("X-CSRF-Token", "staff-csrf")
	req.Header.Set("Origin", "http://example.com")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if org != "" {
		req.Header.Set(orgHeader, org)
	}
	if method != http.MethodGet {
		h := sha256.New()
		h.Write([]byte(method + "\x00" + req.URL.RequestURI() + "\x00" + req.Header.Get("Content-Type") + "\x00" + body))
		id := newID(fx)
		check(fx, fx.srv.putStepUp(id, ssoStepUp{Session: sessionHash(staffToken), Action: hex.EncodeToString(h.Sum(nil)), Verified: true, Created: time.Now(), Expires: time.Now().Add(5 * time.Minute)}))
		req.Header.Set(stepUpHeader, id)
	}
	w := httptest.NewRecorder()
	fx.srv.routes().ServeHTTP(w, req)
	return w
}

// auditSince returns the audit rows written after the row with ID after.
func (fx *orgFixture) auditSince(after int64) []state.BackupAudit {
	fx.t.Helper()
	rows, err := fx.srv.globalStore.RecentBackupAudit(1000)
	check(fx, err)
	var out []state.BackupAudit
	for _, a := range rows {
		if a.ID > after {
			out = append(out, a)
		}
	}
	return out
}

func (fx *orgFixture) lastAudit() int64 {
	var last int64
	for _, a := range fx.auditSince(0) {
		last = max(last, a.ID)
	}
	return last
}

func userOf(_ *orgFixture, m orgMember) string { return m.ID }

func aliasOf(fx *orgFixture, m orgMember) string {
	x, err := fx.srv.ssoLifecycle.AddNativeAlias(context.Background(), fx.srv.stateDir, m.extra, "alias-"+newID(fx)[:8]+"@example.test")
	check(fx, err)
	return x.Address
}

// reservedAliasOf is a released alias of m's, which only a reassign can give out again.
func reservedAliasOf(fx *orgFixture, m orgMember) string {
	address := aliasOf(fx, m)
	_, err := fx.srv.ssoLifecycle.ReleaseNativeAlias(context.Background(), fx.srv.stateDir, address)
	check(fx, err)
	return address
}

// disabledExtraOf is m's extra mailbox, disabled, so that enabling it can succeed.
func disabledExtraOf(fx *orgFixture, m orgMember) string {
	_, err := fx.srv.ssoLifecycle.SetNativeMailboxState(context.Background(), fx.srv.stateDir, m.extra, false)
	check(fx, err)
	return m.extra
}

// staffCases has one case per org-bound administrator route (withOrgAdmin). The body may
// name {id}, the object, and {other}, A's second member's extra mailbox. A query case
// names the object in ?user=.
var staffCases = map[string]struct {
	seed    func(*orgFixture, orgMember) string
	missing string
	body    string
	query   bool
}{
	"GET /api/users":                                    {seed: userOf, missing: missingUUID, query: true},
	"GET /api/admin/mailboxes":                          {seed: userOf, missing: missingUUID, query: true},
	"GET /api/admin/mail-addresses":                     {seed: userOf, missing: missingUUID, query: true},
	"PUT /api/users/{id}":                               {seed: userOf, missing: missingUUID, body: `{"role":"user"}`},
	"POST /api/users/{id}/deactivate":                   {seed: userOf, missing: missingUUID},
	"POST /api/users/{id}/reactivate":                   {seed: userOf, missing: missingUUID},
	"POST /api/users/{id}/clear-mfa":                    {seed: userOf, missing: missingUUID},
	"PUT /api/users/{id}/key-setup":                     {seed: userOf, missing: missingUUID, body: `{"required":false}`},
	"PUT /api/users/{id}/send-quota":                    {seed: userOf, missing: missingUUID, body: `{"sendQuota":null}`},
	"POST /api/admin/mailboxes":                         {seed: userOf, missing: missingUUID, body: `{"user":"{id}","address":"staff-made-{n}@example.test"}`},
	"POST /api/admin/mailboxes/{id}/disable":            {seed: extraOf, missing: mailboxHeaderCase.missing, body: `{}`},
	"POST /api/admin/mailboxes/{id}/enable":             {seed: disabledExtraOf, missing: mailboxHeaderCase.missing, body: `{}`},
	"POST /api/admin/mail-addresses":                    {seed: extraOf, missing: mailboxHeaderCase.missing, body: `{"mailbox":"{id}","address":"staff-alias-{n}@example.test"}`},
	"DELETE /api/admin/mail-addresses/{address}":        {seed: aliasOf, missing: "never-issued@example.test", body: `{}`},
	"POST /api/admin/mail-addresses/{address}/reassign": {seed: reservedAliasOf, missing: "never-issued@example.test", body: `{"mailbox":"{other}"}`},
}

// TestCrossOrgMatrixStaff is the matrix's staff variant: as a staff administrator with org A
// selected, every org-bound route given B's object answers byte for byte as for a missing
// one, changes nothing of B's, and writes audit rows naming A only.
func TestCrossOrgMatrixStaff(t *testing.T) {
	fx, logs, staff := staffFixture(t)
	covered := 0
	for _, r := range crossOrgRoutes(t) {
		if r.class != "org-bound" {
			continue
		}
		c, ok := staffCases[r.key()]
		if !ok {
			t.Errorf("org-bound admin route %q has no case in staffCases", r.key())
			continue
		}
		covered++
		t.Run(r.key(), func(t *testing.T) {
			fx.t = t
			n := 0
			call := func(id string) *httptest.ResponseRecorder {
				n++
				path := r.pattern
				if c.query {
					path += "?user=" + id
				} else if i := strings.Index(path, "{"); i >= 0 {
					path = path[:i] + id + path[i+strings.Index(path[i:], "}")+1:]
				}
				body := strings.NewReplacer("{id}", id, "{other}", fx.other.extra, "{n}", strconv.Itoa(n)).Replace(c.body)
				return fx.staffCall(staff, r.method, path, body, orgA)
			}
			defer func() {
				// The controls deactivate and disable A's objects; later cases need them live.
				_, _ = fx.srv.users.Reactivate(fx.a.ID)
				_, _ = fx.srv.ssoLifecycle.SetNativeMailboxState(context.Background(), fx.srv.stateDir, fx.a.extra, true)
			}()
			own, foreign := c.seed(fx, fx.a), c.seed(fx, fx.b)
			if w := call(own); w.Code >= 300 {
				t.Errorf("control with A's own object: %d %s", w.Code, w.Body)
			}
			before, mark := fx.orgDigest(fx.b), fx.lastAudit()
			logs.take()
			gotB := normalized(call(foreign), foreign)
			lines, rows := logs.take(), fx.auditSince(mark)
			gotMissing := normalized(call(c.missing), c.missing)
			if gotB != gotMissing {
				t.Errorf("B's object answered unlike a missing one:\nB:       %q\nmissing: %q", gotB, gotMissing)
			}
			if after := fx.orgDigest(fx.b); after != before {
				t.Errorf("the call with B's object changed B's stores: %s", orgDigestDiff(before, after))
			}
			if len(rows) == 0 {
				t.Error("the call with B's object wrote no audit row")
			}
			for _, a := range rows {
				if a.Target != "org:"+orgA || a.Actor != staff.ID || a.Details != `{"route":"`+r.key()+`"}` {
					t.Errorf("audit row %+v, want target org:%s and the route only", a, orgA)
				}
			}
			for _, name := range []string{fx.b.ID, fx.b.SSOSub, fx.b.extra, fx.b.Username, foreign, orgB} {
				if strings.Contains(lines, name) {
					t.Errorf("a log line names B (%q): %s", name, lines)
				}
			}
		})
	}
	if covered == 0 {
		t.Fatal("no org-bound admin route found")
	}
}

// The lists name only the selected org's accounts and mailboxes.
func TestStaffListsKeepTheSelectedOrg(t *testing.T) {
	fx, _, staff := staffFixture(t)
	for _, path := range []string{"/api/users", "/api/admin/mailboxes", "/api/admin/mail-addresses"} {
		w := fx.staffCall(staff, http.MethodGet, path, "", orgA)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, fx.a.ID) || !strings.Contains(body, fx.other.ID) {
			t.Fatalf("%s with org A: %d %s", path, w.Code, body)
		}
		for _, name := range []string{fx.b.ID, fx.b.extra, fx.b.Username, staff.ID} {
			if strings.Contains(body, name) {
				t.Errorf("%s with org A names %q: %s", path, name, body)
			}
		}
	}
	if w := fx.staffCall(staff, http.MethodGet, "/api/users", "", staffOrg); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), staff.ID) || strings.Contains(w.Body.String(), fx.a.ID) {
		t.Fatalf("the staff org: %d %s", w.Code, w.Body)
	}
}

// X-Ky-Org: missing, malformed, repeated or unknown is refused before the handler and
// before any audit row; a customer member's header changes nothing.
func TestStaffOrgSelection(t *testing.T) {
	fx, _, staff := staffFixture(t)
	send := func(values ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: staffToken})
		for _, v := range values {
			req.Header.Add(orgHeader, v)
		}
		w := httptest.NewRecorder()
		fx.srv.routes().ServeHTTP(w, req)
		return w
	}
	fx.staffCall(staff, http.MethodGet, "/api/users", "", orgA) // mints the session
	mark := fx.lastAudit()
	for _, c := range []struct {
		values []string
		code   int
		want   string
	}{
		{nil, 400, "org_required"},
		{[]string{""}, 400, "org_required"},
		{[]string{"not-an-org"}, 400, "org_invalid"},
		{[]string{strings.ToUpper(orgA)}, 400, "org_invalid"},
		{[]string{orgA + " "}, 400, "org_invalid"},
		{[]string{strings.Repeat("a", 1<<12)}, 400, "org_invalid"},
		{[]string{orgA, orgA}, 400, "org_invalid"},
		{[]string{orgA, orgB}, 400, "org_invalid"},
		{[]string{"11111111-1111-4111-8111-111111111111"}, 404, "org_unknown"},
	} {
		w := send(c.values...)
		var got struct{ Error string }
		if w.Code != c.code || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error != c.want {
			t.Errorf("X-Ky-Org %q: %d %s, want %d %s", c.values, w.Code, w.Body, c.code, c.want)
		}
	}
	if rows := fx.auditSince(mark); len(rows) != 0 {
		t.Errorf("refused selections wrote audit rows: %+v", rows)
	}

	// A customer member: every admin route stays 403 with any header, and a member route
	// ignores it.
	me := func(header map[string]string) string {
		return normalized(fx.call(fx.a, http.MethodGet, "/api/auth/me", "", header), "\x00")
	}
	if me(map[string]string{orgHeader: orgB}) != me(nil) {
		t.Error("a member's X-Ky-Org changed /api/auth/me")
	}

	// The audit log is required: without it nothing runs.
	fx.srv.backup = nil
	if w := fx.staffCall(staff, http.MethodPost, "/api/users/"+fx.a.ID+"/deactivate", "", orgA); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no audit log: %d %s", w.Code, w.Body)
	}
	if u, _ := fx.srv.users.Get(fx.a.ID); !u.Active {
		t.Fatal("a staff write ran without an audit row")
	}
}

// Every administrator route, called as a customer member, gets today's non-administrator
// refusal, with or without X-Ky-Org, and changes nothing.
func TestEveryAdminRouteRefusesACustomerMember(t *testing.T) {
	fx, _, _ := staffFixture(t)
	beforeA, beforeB, mark := fx.orgDigest(fx.a), fx.orgDigest(fx.b), fx.lastAudit()
	admin := 0
	for _, r := range crossOrgRoutes(t) {
		if !r.admin {
			continue
		}
		admin++
		path := strings.NewReplacer("{id}", fx.b.ID, "{domain}", "example.test", "{address}", "a@example.test", "{gateway}", "g", "{token}", "t").Replace(r.pattern)
		for _, h := range []map[string]string{nil, {orgHeader: orgA}, {orgHeader: orgB}, {orgHeader: staffOrg}} {
			w := fx.call(fx.a, r.method, path, `{}`, h)
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != `{"error":"admin access required"}` {
				t.Errorf("%s as a customer member with %v: %d %s", r.key(), h, w.Code, w.Body)
			}
		}
	}
	if admin < 70 {
		t.Fatalf("found only %d administrator routes", admin)
	}
	if fx.orgDigest(fx.a) != beforeA || fx.orgDigest(fx.b) != beforeB {
		t.Error("a refused administrator call changed a member's stores")
	}
	if rows := fx.auditSince(mark); len(rows) != 0 {
		t.Errorf("a customer member's calls wrote audit rows: %+v", rows)
	}
}

// K6: the refused administrator routes and the member settings multi mode has no place for
// answer 404, as an unknown route does.
func TestTenancyK6RefusalsAnswer404(t *testing.T) {
	fx, _, staff := staffFixture(t)
	refused := 0
	for _, r := range crossOrgRoutes(t) {
		if r.class != "refused" {
			continue
		}
		refused++
		path := strings.ReplaceAll(r.pattern, "{id}", fx.a.ID)
		if w := fx.staffCall(staff, r.method, path, `{}`, orgA); w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
			t.Errorf("%s as staff: %d %s", r.key(), w.Code, w.Body)
		}
	}
	if refused != 9 {
		t.Errorf("found %d refused routes, want 9", refused)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/auth/login"}, {"GET", "/api/auth/login-params?username=x"},
		{"POST", "/api/notifications/review-pairing"},
		{"GET", "/api/imap/config"}, {"POST", "/api/imap/config"}, {"DELETE", "/api/imap/config"}, {"POST", "/api/imap/test"},
	} {
		if w := fx.call(fx.a, c.method, c.path, `{}`, nil); w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != "404 page not found" {
			t.Errorf("%s %s in multi mode: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	sso := fx.srv.ssoStore.Load()
	for _, on := range []bool{true, false} {
		sso.AutoProvision = on
		raw, _ := json.Marshal(sso)
		w := fx.staffCall(staff, http.MethodPut, "/api/admin/sso", string(raw), "")
		if on && w.Code != http.StatusNotFound || !on && w.Code != http.StatusOK {
			t.Errorf("AutoProvision %v in multi mode: %d %s", on, w.Code, w.Body)
		}
	}
	if fx.srv.ssoStore.Load().AutoProvision {
		t.Error("AutoProvision was stored in multi mode")
	}
}

// K7 is checked where admin routes are admitted, not only in the users store.
func TestIsStaff(t *testing.T) {
	single, multi := &Server{}, newTestServer(t)
	makeMultiTenant(t, multi)
	if !single.isStaff(AuthContext{}) {
		t.Error("single mode refused an administrator")
	}
	for org, want := range map[string]bool{staffOrg: true, orgA: false, "": false} {
		if got := multi.isStaff(AuthContext{OrgID: org}); got != want {
			t.Errorf("isStaff(%q) = %v, want %v", org, got, want)
		}
	}
}

// Single mode: X-Ky-Org is ignored and no staff audit row is written.
func TestSingleTenantIgnoresTheOrgHeader(t *testing.T) {
	srv, admin, member := adminAndMember(t)
	withAuditLog(t, srv)
	get := func(header string) string {
		req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
		authRequestAs(srv, req, admin.ID)
		if header != "" {
			req.Header.Set(orgHeader, header)
		}
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), member.ID) {
			t.Fatalf("GET /api/users with %q: %d %s", header, w.Code, w.Body)
		}
		return normalized(w, "\x00")
	}
	if want := get(""); get(orgA) != want || get("not-an-org") != want {
		t.Error("X-Ky-Org changed a single-tenant answer")
	}
	if rows, err := srv.globalStore.RecentBackupAudit(10); err != nil || len(rows) != 0 {
		t.Errorf("single mode wrote audit rows: %+v %v", rows, err)
	}
}
