package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/sso/ssotest"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	orgA     = "1a2b3c4d-0000-4000-8000-00000000000a"
	orgB     = "1a2b3c4d-0000-4000-8000-00000000000b"
	staffOrg = "1a2b3c4d-0000-4000-8000-0000000000ff"
)

// makeMultiTenant gives srv the side store a multi first start creates (PR 1's
// tenancy.Check). Startup still refuses multi, so tests reach the multi paths here.
// useTenancy makes s multi-tenant as PR 7's startup must: the side store, the staff org,
// and the address tombstones every allocation consults first (K10).
func (s *Server) useTenancy(db *sql.DB, staffOrg string) {
	s.tenancy, s.staffOrg, s.users.StaffOrg = db, staffOrg, staffOrg
	if s.ssoLifecycle != nil {
		s.ssoLifecycle.Tombstoned = func(address string) (bool, error) {
			key, err := tenancy.TombstoneKey(context.Background(), db, config.SecretDir())
			if err != nil {
				return false, err
			}
			return tenancy.Tombstoned(context.Background(), db, tenancy.Tombstone(key, address))
		}
	}
}

func makeMultiTenant(t *testing.T, srv *Server) *sql.DB {
	t.Helper()
	db, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv.useTenancy(db, staffOrg)
	// The fixtures' customer orgs are served unless a test changes them (PR 5).
	setOrg(t, db, orgA, "active", true, `{}`)
	setOrg(t, db, orgB, "active", true, `{}`)
	return db
}

// multiSSOServer is a multi-tenant server with a test IdP that also receives
// signed directory events, the only way multi mode creates an account.
func multiSSOServer(t *testing.T) (*Server, *ssotest.IdP, *sql.DB) {
	t.Helper()
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	return srv, idp, makeMultiTenant(t, srv)
}

func orgUser(sub, org string, active bool, roles ...any) map[string]any {
	u := scimUser(sub, "u_"+sub, active, roles...)
	if org != "" {
		u["org_id"] = org
	}
	return u
}

func rowOrg(t *testing.T, db *sql.DB, query string, key ...any) string {
	t.Helper()
	var org string
	if err := db.QueryRow(query, key...).Scan(&org); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return org
}

func userOrgRow(t *testing.T, db *sql.DB, id string) string {
	return rowOrg(t, db, `SELECT org_id FROM user_orgs WHERE user_id = ?`, id)
}

func subjectOrgRow(t *testing.T, srv *Server, db *sql.DB, sub string) string {
	return rowOrg(t, db, `SELECT org_id FROM subject_orgs WHERE issuer = ? AND subject = ?`, srv.ssoStore.Load().IssuerURL, sub)
}

// provisionMember creates sub's account in org through a signed event and checks all
// three records: users.json OrgID, user_orgs and subject_orgs.
func provisionMember(t *testing.T, srv *Server, db *sql.DB, sub, org string, roles ...any) users.User {
	t.Helper()
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-"+sub, 1, orgUser(sub, org, true, roles...)))
	u, err := srv.users.GetBySSOSub(sub)
	if err != nil {
		t.Fatal(err)
	}
	if u.OrgID != org || userOrgRow(t, db, u.ID) != org || subjectOrgRow(t, srv, db, sub) != org {
		t.Fatalf("%s: OrgID %q, user_orgs %q, subject_orgs %q, want %q", sub, u.OrgID, userOrgRow(t, db, u.ID), subjectOrgRow(t, srv, db, sub), org)
	}
	return u
}

// A directory event without a valid org_id, with a bad signature or oversize, is
// refused before anything is written.
func TestTenancySyncRefusesEventsWithoutAValidOrg(t *testing.T) {
	srv := newDirectoryTestServer(t)
	db := makeMultiTenant(t, srv)
	for name, org := range map[string]any{
		"missing":   nil,
		"empty":     "",
		"uppercase": strings.ToUpper(orgA),
		"braced":    "{" + orgA + "}",
		"no dashes": strings.ReplaceAll(orgA, "-", ""),
		"padded":    " " + orgA,
		"number":    7,
		"object":    map[string]any{"id": orgA},
	} {
		u := scimUser("s-"+name, "u", true)
		if org != nil {
			u["org_id"] = org
		}
		if rec := postDirectory(t, srv, testSyncKey, "user.created", "ev-"+name, 1, u); rec.Code != http.StatusBadRequest {
			t.Errorf("%s org_id: status %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := postDirectory(t, srv, "wrong-secret-also-32-bytes-long!", "user.created", "ev-sig", 1, orgUser("s-sig", orgA, true)); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong signature: status %d, want 401", rec.Code)
	}
	big := orgUser("s-big", orgA, true)
	big["userName"] = strings.Repeat("a", maxSyncEventBytes)
	if rec := postDirectory(t, srv, testSyncKey, "user.created", "ev-big", 1, big); rec.Code != http.StatusBadRequest {
		t.Errorf("oversize: status %d, want 400", rec.Code)
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM subject_orgs) + (SELECT COUNT(*) FROM user_orgs)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused events wrote %d bindings (%v)", n, err)
	}
	all, _ := srv.users.List()
	for _, u := range all {
		if u.SSOSub != "" {
			t.Fatalf("a refused event provisioned %+v", u)
		}
	}
}

// The first event binds the subject; a later, replayed or reordered event naming
// another org is refused before the revision fence and changes nothing.
func TestTenancySyncCannotMoveASubject(t *testing.T) {
	srv := newDirectoryTestServer(t)
	db := makeMultiTenant(t, srv)
	const sub = "member-1"
	u := provisionMember(t, srv, db, sub, orgA)
	issuer := srv.ssoStore.Load().IssuerURL
	revisionOf := func() int64 {
		d, _, err := srv.ssoLifecycle.Directory(issuer, sub)
		if err != nil {
			t.Fatal(err)
		}
		return d.Revision
	}

	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-"+sub, 1, orgUser(sub, orgA, true))); got != "already_applied" {
		t.Fatalf("replay: %q", got)
	}
	for _, tc := range []struct {
		name     string
		revision int
		active   bool
	}{{"newer", 2, true}, {"newer disable", 3, false}, {"reordered older", 1, true}} {
		rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-b-"+tc.name, tc.revision, orgUser(sub, orgB, tc.active))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s event for org B: status %d, want 422: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	after, err := srv.users.Get(u.ID)
	if err != nil || after.OrgID != orgA || !after.Active || userOrgRow(t, db, u.ID) != orgA || subjectOrgRow(t, srv, db, sub) != orgA || revisionOf() != 1 {
		t.Fatalf("org B events changed the account: %+v %v, revision %d", after, err, revisionOf())
	}
	// The same org still applies, at the next revision the fence expects.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-a-2", 2, orgUser(sub, orgA, true))); got != "applied" {
		t.Fatalf("org A update: %q", got)
	}

	// A subject bound by an event that made no account stays bound.
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.deleted", "ev-gone", 1, orgUser("gone", orgA, false)))
	if subjectOrgRow(t, srv, db, "gone") != orgA {
		t.Fatal("an inactive first event did not bind the subject")
	}
	if rec := postDirectory(t, srv, testSyncKey, "user.created", "ev-gone-b", 2, orgUser("gone", orgB, true)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bound subject in org B: status %d", rec.Code)
	}
	if _, err := srv.users.GetBySSOSub("gone"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("an org B event created an account: %v", err)
	}
}

// K's write order converges after a crash between steps: subject_orgs written with no
// account, or an account written with no user_orgs row.
func TestTenancySyncRepairsAfterACrashBetweenSteps(t *testing.T) {
	srv := newDirectoryTestServer(t)
	db := makeMultiTenant(t, srv)
	issuer := srv.ssoStore.Load().IssuerURL

	if err := tenancy.BindSubject(context.Background(), db, issuer, "half", orgA); err != nil {
		t.Fatal(err)
	}
	u := provisionMember(t, srv, db, "half", orgA)

	if _, err := db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-half-2", 2, orgUser("half", orgA, true)))
	if userOrgRow(t, db, u.ID) != orgA {
		t.Fatal("a retried event did not repair user_orgs")
	}
	if again, _ := srv.users.GetBySSOSub("half"); again.ID != u.ID {
		t.Fatalf("the retry minted a second ID: %s, then %s", u.ID, again.ID)
	}

	// With the subject's row lost (tenancy.db is not backed up yet), an event for another
	// org is refused against the account and binds nothing; the account's org then binds.
	if _, err := db.Exec(`DELETE FROM subject_orgs WHERE subject = ?`, "half"); err != nil {
		t.Fatal(err)
	}
	if rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-half-b", 3, orgUser("half", orgB, true)); rec.Code != http.StatusUnprocessableEntity || subjectOrgRow(t, srv, db, "half") != "" {
		t.Fatalf("org B against an org A account: status %d, subject_orgs %q", rec.Code, subjectOrgRow(t, srv, db, "half"))
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-half-a", 3, orgUser("half", orgA, true)))
	if subjectOrgRow(t, srv, db, "half") != orgA {
		t.Fatal("the account's own org did not rebind the subject")
	}

	// A user_orgs row naming another org is a disagreement: refused, not repaired.
	if _, err := db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.BindUser(context.Background(), db, u.ID, orgB); err != nil {
		t.Fatal(err)
	}
	if rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-half-4", 4, orgUser("half", orgA, true)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disagreeing records: status %d, want 422", rec.Code)
	}
}

// K7 through the directory: kypost.admin makes an administrator only in the staff org.
func TestTenancyAdminNeedsTheStaffOrg(t *testing.T) {
	srv := newDirectoryTestServer(t)
	db := makeMultiTenant(t, srv)
	issuer := srv.ssoStore.Load().IssuerURL

	customer := provisionMember(t, srv, db, "customer", orgA, "kypost.admin")
	if customer.Role != users.RoleUser {
		t.Fatalf("a customer naming kypost.admin got role %q", customer.Role)
	}
	if d, _, _ := srv.ssoLifecycle.Directory(issuer, "customer"); d.Resource == nil || sso.HasAdminRole(d.Resource.Roles) {
		t.Fatal("the stored desired state kept the customer's administrator role")
	}

	staff := provisionMember(t, srv, db, "staff-1", staffOrg, "kypost.admin")
	if staff.Role != users.RoleAdmin {
		t.Fatalf("staff administrator got role %q", staff.Role)
	}

	// A staff-org subject without the role gets no account.
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-plain", 1, orgUser("staff-plain", staffOrg, true)))
	if _, err := srv.users.GetBySSOSub("staff-plain"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("a staff-org member got an account: %v", err)
	}

	// Losing the role deactivates, regaining it reactivates.
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-s1-2", 2, orgUser("staff-1", staffOrg, true)))
	if u, _ := srv.users.Get(staff.ID); u.Active || u.Role != users.RoleAdmin {
		t.Fatalf("staff without kypost.admin: active %v role %q, want deactivated admin", u.Active, u.Role)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-s1-3", 3, orgUser("staff-1", staffOrg, true, "kypost.admin")))
	if u, _ := srv.users.Get(staff.ID); !u.Active || u.Role != users.RoleAdmin {
		t.Fatalf("staff regaining kypost.admin: active %v role %q", u.Active, u.Role)
	}

	// The last administrator cannot be removed this way.
	all, _ := srv.users.List()
	for _, u := range all {
		if u.Role == users.RoleAdmin && u.ID != staff.ID {
			if _, err := srv.users.Deactivate(u.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-s1-4", 4, orgUser("staff-1", staffOrg, true)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("removing the last staff administrator: status %d, want 422", rec.Code)
	}
	if u, _ := srv.users.Get(staff.ID); !u.Active {
		t.Fatal("the last staff administrator was deactivated")
	}
}

func signInRecorder(t *testing.T, srv *Server, idp *ssotest.IdP, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	idp.SetClaims(claims)
	return runSSOFlow(t, srv, idp, nil, false)
}

// The browser sign-in needs the token's org_id, and it must be the account's.
func TestTenancySignInBindsTheTokenOrg(t *testing.T) {
	srv, idp, db := multiSSOServer(t)
	member := provisionMember(t, srv, db, "alice", orgA)
	provisionMember(t, srv, db, "boss", staffOrg, "kypost.admin")
	claims := func(sub string, org any, extra ...any) map[string]any {
		c := map[string]any{"sub": sub, "preferred_username": "u_" + sub, "roles": []string{}}
		if org != nil {
			c["org_id"] = org
		}
		for i := 0; i+1 < len(extra); i += 2 {
			c[extra[i].(string)] = extra[i+1]
		}
		return c
	}

	c := signInWith(t, srv, idp, claims("alice", orgA))
	if effectiveRole(t, srv, c) != users.RoleUser {
		t.Fatal("member is not a user")
	}
	for name, tc := range map[string]struct {
		claims map[string]any
		status int
	}{
		"missing org_id":       {claims("alice", nil), http.StatusBadGateway},
		"malformed org_id":     {claims("alice", "org-a"), http.StatusBadGateway},
		"non-canonical org_id": {claims("alice", strings.ToUpper(orgA)), http.StatusBadGateway},
		"numeric org_id":       {claims("alice", 1), http.StatusBadGateway},
		// Export-only tokens sign in only while the org is deletion_scheduled (PR 6b).
		"export-only token":  {claims("alice", orgA, "export_only", true), http.StatusForbidden},
		"string export_only": {claims("alice", orgA, "export_only", "false"), http.StatusForbidden},
		"another org":        {claims("alice", orgB), http.StatusForbidden},
		"the staff org":      {claims("alice", staffOrg, "roles", []string{"kypost.admin"}), http.StatusForbidden},
		"unprovisioned":      {claims("nobody", orgA), http.StatusForbidden},
	} {
		rec := signInRecorder(t, srv, idp, tc.claims)
		if rec.Code != tc.status || sessionCookieFrom(rec) != nil {
			t.Errorf("%s: status %d, session %v; want %d and none", name, rec.Code, sessionCookieFrom(rec) != nil, tc.status)
		}
	}
	if _, err := srv.users.GetBySSOSub("nobody"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("multi mode provisioned an account from a token")
	}
	// An IdP that writes an absent boolean as null, or as false, is not export-only.
	for _, v := range []any{nil, false} {
		if effectiveRole(t, srv, signInWith(t, srv, idp, claims("alice", orgA, "export_only", v))) != users.RoleUser {
			t.Fatalf("export_only %v: member is not a user", v)
		}
	}

	// A customer token naming kypost.admin is still a member's session.
	c = signInWith(t, srv, idp, claims("alice", orgA, "roles", []string{"kypost.admin"}))
	if effectiveRole(t, srv, c) != users.RoleUser {
		t.Fatal("a customer token made an administrator")
	}
	c = signInWith(t, srv, idp, claims("boss", staffOrg, "roles", []string{"kypost.admin"}))
	if effectiveRole(t, srv, c) != users.RoleAdmin {
		t.Fatal("the staff administrator is not an administrator")
	}
	// No path makes a customer account an administrator, and a staff session needs the
	// role claim itself: no group fallback.
	if _, err := srv.users.SetRole(member.ID, users.RoleAdmin); !errors.Is(err, users.ErrCustomerAdmin) {
		t.Fatalf("a customer account was promoted: %v", err)
	}
	c = signInWith(t, srv, idp, map[string]any{"sub": "boss", "preferred_username": "u_boss", "groups": []string{"admin"}, "org_id": staffOrg})
	if effectiveRole(t, srv, c) != users.RoleUser {
		t.Fatal("a staff token without the role claim made an administrator session")
	}

	// A missing user_orgs row is repaired at sign-in; one naming another org refuses.
	if _, err := db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, member.ID); err != nil {
		t.Fatal(err)
	}
	signInWith(t, srv, idp, claims("alice", orgA))
	if userOrgRow(t, db, member.ID) != orgA {
		t.Fatal("sign-in did not repair user_orgs")
	}
	if _, err := db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, member.ID); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.BindUser(context.Background(), db, member.ID, orgB); err != nil {
		t.Fatal(err)
	}
	if rec := signInRecorder(t, srv, idp, claims("alice", orgA)); rec.Code != http.StatusForbidden {
		t.Fatalf("disagreeing user_orgs: status %d, want 403", rec.Code)
	}

	// So is a subject_orgs row naming another org.
	bob := provisionMember(t, srv, db, "bob", orgA)
	if _, err := db.Exec(`DELETE FROM subject_orgs WHERE subject = ?`, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.BindSubject(context.Background(), db, bob.SSOIssuer, "bob", orgB); err != nil {
		t.Fatal(err)
	}
	if rec := signInRecorder(t, srv, idp, claims("bob", orgA)); rec.Code != http.StatusForbidden {
		t.Fatalf("disagreeing subject_orgs: status %d, want 403", rec.Code)
	}
}

// Device sign-on applies the same rule before the pairing link is minted.
func TestTenancyDeviceSignOnBindsTheTokenOrg(t *testing.T) {
	srv, idp := nativeSignOnServer(t)
	srv.pairingSecret = testSyncKey
	db := makeMultiTenant(t, srv)
	provisionMember(t, srv, db, "sso-sub-12345", orgA)
	for i, tc := range []struct {
		org    any
		extra  map[string]any
		status int
	}{
		{nil, nil, http.StatusForbidden},
		{"not-an-org", nil, http.StatusForbidden},
		{orgB, nil, http.StatusForbidden},
		{orgA, map[string]any{"export_only": true}, http.StatusForbidden},
		{orgA, nil, http.StatusOK},
	} {
		extra := map[string]any{"jti": "jti-org-" + string(rune('a'+i))}
		if tc.org != nil {
			extra["org_id"] = tc.org
		}
		for k, v := range tc.extra {
			extra[k] = v
		}
		idp.SetClaims(deviceClaims(extra))
		rec := httptest.NewRecorder()
		srv.handleNativeSignOn(rec, signOnRequest(idp.IDToken()))
		if rec.Code != tc.status || (tc.status != http.StatusOK && strings.Contains(rec.Body.String(), "native-pair")) {
			t.Errorf("case %d (org %v): status %d, want %d: %s", i, tc.org, rec.Code, tc.status, rec.Body.String())
		}
	}
}

// A logout token in multi mode needs org_id and sub, and must name the account's org.
func TestTenancyLogoutNeedsTheAccountOrg(t *testing.T) {
	srv, idp, db := multiSSOServer(t)
	provisionMember(t, srv, db, "alice", orgA)
	idp.SetClaims(map[string]any{"sub": "alice", "preferred_username": "u_alice", "sid": "sid-1", "roles": []string{}, "org_id": orgA})
	rec := runSSOFlow(t, srv, idp, nil, false)
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body.String())
	}
	withOrg := func(org any) func(map[string]any) {
		return func(m map[string]any) {
			if org != nil {
				m["org_id"] = org
			}
		}
	}
	for name, token := range map[string]string{
		"missing org_id": idp.LogoutToken(t, "alice", "sid-1", nil),
		"malformed":      idp.LogoutToken(t, "alice", "sid-1", withOrg(strings.ToUpper(orgA))),
		"another org":    idp.LogoutToken(t, "alice", "sid-1", withOrg(orgB)),
		"sid only":       idp.LogoutToken(t, "", "sid-1", withOrg(orgA)),
	} {
		if rec := postLogout(srv, logoutForm(token)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
		if !sessionAlive(srv, c) {
			t.Fatalf("%s ended the session", name)
		}
	}
	if rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "stranger", "sid-x", withOrg(orgA)))); rec.Code != http.StatusOK {
		t.Fatalf("logout for an unknown subject: %d", rec.Code)
	}
	if rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "alice", "sid-1", withOrg(orgA)))); rec.Code != http.StatusOK || sessionAlive(srv, c) {
		t.Fatalf("logout for the account's org: status %d, alive %v", rec.Code, sessionAlive(srv, c))
	}
}

// Single mode parses none of it: tokens and events carrying org_id, of any type, and
// export_only behave as before, and nothing tenancy-shaped is written.
func TestSingleTenantIgnoresOrgClaims(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	signInWith(t, srv, idp, map[string]any{"sub": "plain", "preferred_username": "plain", "roles": []string{}, "org_id": 7, "export_only": "yes"})
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-single", 1, orgUser("synced", "not-an-org", true)))
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-single-num", 1, orgUser("synced-2", "", true)))
	rec := postLogout(srv, logoutForm(idp.LogoutToken(t, "plain", "", func(m map[string]any) { m["org_id"] = orgB })))
	if rec.Code != http.StatusOK {
		t.Fatalf("single-mode logout with org_id: %d", rec.Code)
	}
	for _, sub := range []string{"plain", "synced", "synced-2"} {
		if u, err := srv.users.GetBySSOSub(sub); err != nil || u.OrgID != "" {
			t.Fatalf("%s: %+v %v", sub, u, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(srv.configDir, "users.json"))
	if err != nil || bytes.Contains(raw, []byte("orgId")) || bytes.Contains(raw, []byte("org_id")) {
		t.Fatalf("users.json carries an org (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(srv.stateDir, tenancy.File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("single mode created %s: %v", tenancy.File, err)
	}
}

// SSO link and unlink are refused in multi mode: an org-bound account's subject is write-once.
func TestTenancyRefusesSSOLinkAndUnlink(t *testing.T) {
	srv, _, db := multiSSOServer(t)
	member := provisionMember(t, srv, db, "alice", orgA)
	for name, h := range map[string]http.HandlerFunc{"link start": srv.handleSSOLinkStart, "unlink": srv.handleSSOUnlink} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		authRequestAs(srv, req, member.ID)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.linkSSOIdentity(rec, httptest.NewRequest(http.MethodGet, "/", nil), member.ID, &sso.SSOTokenClaims{Issuer: member.SSOIssuer, Sub: "other"})
	if u, _ := srv.users.Get(member.ID); rec.Code != http.StatusNotFound || u.SSOSub != "alice" {
		t.Fatalf("link: status %d, subject %q", rec.Code, u.SSOSub)
	}
	if err := srv.users.UnlinkSSO(member.ID); !errors.Is(err, users.ErrOrgIDWriteOnce) {
		t.Fatalf("the store unlinked an org-bound account: %v", err)
	}
}

// The hosted member path: native allocation carries the subject's org, a customer naming
// kypost.admin still gets a member's mailbox, and a missing user_orgs row is repaired by
// the next event.
func TestTenancyNativeMemberProvisioning(t *testing.T) {
	srv := newNativeRuntimeServer(t)
	db := makeMultiTenant(t, srv)
	event := func(sub string, revision int, roles ...any) map[string]any {
		u := orgUser(sub, orgA, true, roles...)
		u["emails"] = []map[string]any{{"value": sub + "@example.test", "primary": true}}
		u["meta"] = map[string]any{"version": fmt.Sprintf(`W/"%d"`, revision)}
		return u
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "n-1", 1, event("one", 1)))
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "n-2", 1, event("two", 1, "kypost.admin")))
	for _, sub := range []string{"one", "two"} {
		u, err := srv.users.GetBySSOSub(sub)
		if err != nil || u.NativeMailboxSource == "" || u.Role != users.RoleUser || u.OrgID != orgA || userOrgRow(t, db, u.ID) != orgA {
			t.Fatalf("%s: %+v %v, user_orgs %q", sub, u, err, userOrgRow(t, db, u.ID))
		}
	}
	one, _ := srv.users.GetBySSOSub("one")
	if _, err := db.Exec(`DELETE FROM user_orgs WHERE user_id = ?`, one.ID); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "n-1b", 2, event("one", 2)))
	if userOrgRow(t, db, one.ID) != orgA {
		t.Fatal("the next event did not repair user_orgs")
	}
	if again, _ := srv.users.GetBySSOSub("one"); again.ID != one.ID {
		t.Fatal("re-provisioning minted a second ID")
	}
}

// A subject whose account another issuer owns can never apply, so both modes refuse
// it with a non-retryable 422 and bind nothing.
func TestDirectoryRefusesASubjectOfAnotherIssuer(t *testing.T) {
	for _, multi := range []bool{false, true} {
		srv := newDirectoryTestServer(t)
		var db *sql.DB
		if multi {
			db = makeMultiTenant(t, srv)
		}
		u, err := srv.users.Create(context.Background(), "elsewhere", linkTestPassword, users.RoleUser)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.users.LinkSSOIssuer("https://other-idp.example", u.ID, "other-sub", "elsewhere", ""); err != nil {
			t.Fatal(err)
		}
		rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-other", 1, orgUser("other-sub", orgA, true))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("multi=%v: status %d, want 422: %s", multi, rec.Code, rec.Body.String())
		}
		if multi && subjectOrgRow(t, srv, db, "other-sub") != "" {
			t.Fatal("a refused event bound the subject")
		}
	}
}
