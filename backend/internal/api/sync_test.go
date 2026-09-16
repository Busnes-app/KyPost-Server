package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busness-app/ky-primitives/syncauth"

	"github.com/Busness-app/kypost-server/backend/internal/sso"
	"github.com/Busness-app/kypost-server/backend/internal/users"
)

const testSyncKey = "directory-sync-secret-32-bytes!!"

// scimUser builds the SCIM User a KySignOn directory event carries.
func scimUser(sub, username string, active bool, roles ...any) map[string]any {
	if roles == nil {
		roles = []any{}
	}
	return map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"id":         sub,
		"externalId": sub,
		"userName":   username,
		"active":     active,
		"roles":      roles,
	}
}

// postDirectory signs one event with key and delivers it. revision fills
// meta.version unless the user already carries one.
func postDirectory(t *testing.T, srv *Server, key, eventType, eventID string, revision int, user map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := user["meta"]; !ok {
		user["meta"] = map[string]any{"version": fmt.Sprintf(`W/"%d"`, revision)}
	}
	body, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sync/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/scim+json")
	if key != "" {
		h, err := syncauth.Sign([]byte(key), time.Now().UTC(), eventType, eventID, body)
		if err != nil {
			t.Fatal(err)
		}
		h.Apply(req)
	}
	srv.handleSyncWebhook(rec, req)
	return rec
}

func directoryStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	s, _ := resp["status"].(string)
	return s
}

// newDirectoryTestServer pairs a server with the directory by pairing secret
// and a configured issuer, without a live provider.
func newDirectoryTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.pairingSecret = testSyncKey
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: "https://idp.example", ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestDirectoryAppliesVersionedStateIdempotently(t *testing.T) {
	srv := newDirectoryTestServer(t)
	const sub = "dir-sub-1"

	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser(sub, "dir_user", true))); got != "applied" {
		t.Fatalf("first create: status %q, want applied", got)
	}
	u, err := srv.users.GetBySSOSub(sub)
	if err != nil || u.Role != users.RoleUser || !u.Active {
		t.Fatalf("provisioned user = %+v, err %v", u, err)
	}

	// The same delivery again, and the same content under a new event id,
	// are both acknowledged without doing anything.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser(sub, "dir_user", true))); got != "already_applied" {
		t.Errorf("retry: status %q, want already_applied", got)
	}
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1b", 1, scimUser(sub, "dir_user", true))); got != "already_applied" {
		t.Errorf("same revision, same content: status %q, want already_applied", got)
	}

	// A newer revision promotes.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-2", 2, scimUser(sub, "dir_user", true, map[string]any{"value": "kypost.admin", "primary": true}))); got != "applied" {
		t.Fatalf("promotion: status %q, want applied", got)
	}
	if u, _ = srv.users.GetBySSOSub(sub); u.Role != users.RoleAdmin {
		t.Fatalf("role after promotion = %q, want admin", u.Role)
	}

	// The fence refuses anything it cannot order: an older revision under a
	// fresh event id, the current revision with a different body, and a
	// reused event id with a different body.
	stale := []struct {
		name, eventID string
		revision      int
		user          map[string]any
	}{
		{"older revision", "ev-3", 1, scimUser(sub, "dir_user", true)},
		{"same revision, different body", "ev-4", 2, scimUser(sub, "dir_user", true)},
		{"reused event id, different body", "ev-2", 5, scimUser(sub, "dir_user", true)},
	}
	for _, tc := range stale {
		if rec := postDirectory(t, srv, testSyncKey, "user.updated", tc.eventID, tc.revision, tc.user); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", tc.name, rec.Code, rec.Body.String())
		}
		if u, _ = srv.users.GetBySSOSub(sub); u.Role != users.RoleAdmin {
			t.Fatalf("%s: a refused event demoted the user", tc.name)
		}
	}

	// Disabling and deleting both keep the account and its data; only access goes.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-6", 3, scimUser(sub, "dir_user", false))); got != "applied" {
		t.Fatalf("disable: status %q", got)
	}
	if u, err = srv.users.GetBySSOSub(sub); err != nil || u.Active || u.Username != "dir_user" {
		t.Fatalf("after disable: user = %+v, err %v; want present, inactive, same name", u, err)
	}
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.deleted", "ev-7", 4, scimUser(sub, "dir_user", false))); got != "applied" {
		t.Fatalf("delete: status %q", got)
	}
	if u, err = srv.users.GetBySSOSub(sub); err != nil || u.Active {
		t.Fatalf("after delete: user = %+v, err %v; want present and inactive", u, err)
	}
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-8", 5, scimUser(sub, "dir_user", true))); got != "applied" {
		t.Fatalf("rehire: status %q", got)
	}
	if u, _ = srv.users.GetBySSOSub(sub); !u.Active || u.Role != users.RoleUser {
		t.Fatalf("after rehire: %+v, want active user", u)
	}

	// A restart is a new store over the same directory; the fence survives it.
	srv.ssoLifecycle = sso.NewLifecycleStore(srv.configDir)
	if rec := postDirectory(t, srv, testSyncKey, "user.deleted", "ev-9", 4, scimUser(sub, "dir_user", false)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stale delete after restart: status = %d, want 422", rec.Code)
	}
	if u, _ = srv.users.GetBySSOSub(sub); !u.Active {
		t.Fatal("a stale delete replayed after restart disabled the user")
	}
}

func TestDirectoryDisableRevokesSessionsAndFencesLogin(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-a1")
	alice, err := srv.users.GetBySSOSub("alice")
	if err != nil {
		t.Fatal(err)
	}

	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-1", 1, scimUser("alice", "alice", false))); got != "applied" {
		t.Fatalf("disable: status %q", got)
	}
	if sessionAlive(srv, cookie) {
		t.Error("disabling the user left their session alive")
	}
	if u, err := srv.users.Get(alice.ID); err != nil || u.Active {
		t.Fatalf("after disable: %+v, %v; want present and inactive", u, err)
	}

	// The provider still issues tokens for the subject; the directory's word
	// outranks them, and blocks auto-provisioning a second account too.
	idp.SetClaims(map[string]any{"sub": "alice", "preferred_username": "alice", "sid": "sid-a2"})
	if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatalf("login for a disabled subject: status = %d, want 403 and no cookie", rec.Code)
	}
	if all, _ := srv.users.List(); len(all) != 2 {
		t.Fatalf("account count = %d after a refused login, want 2 (bootstrap admin and alice)", len(all))
	}

	// Rehired: the same account comes back, and can sign in again.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-2", 2, scimUser("alice", "alice", true))); got != "applied" {
		t.Fatalf("rehire: status %q", got)
	}
	if u, _ := srv.users.Get(alice.ID); !u.Active {
		t.Fatal("rehire did not reactivate the account")
	}
	ssoSignIn(t, srv, idp, "alice", "sid-a3")
}

func TestDirectoryRoleChangeEndsSessionsAndStaleTokens(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	cookie := ssoSignIn(t, srv, idp, "alice", "sid-a1")
	before := time.Now().Add(-10 * time.Second).Unix()

	// Unrelated role names change nothing and refuse nothing.
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-1", 1, scimUser("alice", "alice", true, "notes.admin", map[string]any{"value": "kypost.user"}))); got != "applied" {
		t.Fatalf("unrelated roles: status %q", got)
	}
	if u, _ := srv.users.GetBySSOSub("alice"); u.Role != users.RoleUser {
		t.Fatalf("an unrelated role name granted %q", u.Role)
	}
	if !sessionAlive(srv, cookie) {
		t.Fatal("an event that changed nothing ended the session")
	}

	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-2", 2, scimUser("alice", "alice", true, "kypost.admin"))); got != "applied" {
		t.Fatalf("promotion: status %q", got)
	}
	if u, _ := srv.users.GetBySSOSub("alice"); u.Role != users.RoleAdmin {
		t.Fatalf("role = %q after promotion", u.Role)
	}
	if sessionAlive(srv, cookie) {
		t.Error("a role change left the old session alive")
	}

	// A token issued before the change is stale; a fresh login is fine.
	idp.SetClaims(map[string]any{"sub": "alice", "preferred_username": "alice", "sid": "sid-a2", "iat": before})
	if rec := runSSOFlow(t, srv, idp, nil, false); rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatalf("login with a token issued before the role change: status = %d, want 403", rec.Code)
	}
	ssoSignIn(t, srv, idp, "alice", "sid-a3")
}

func TestDirectoryRefusesUnsignedMalformedAndLastAdminRemoval(t *testing.T) {
	srv := newDirectoryTestServer(t)

	if rec := postDirectory(t, srv, "", "user.created", "ev-1", 1, scimUser("s1", "s1", true)); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned: status = %d, want 401", rec.Code)
	}
	if rec := postDirectory(t, srv, "wrong-secret-also-32-bytes-long!", "user.created", "ev-1", 1, scimUser("s1", "s1", true)); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: status = %d, want 401", rec.Code)
	}
	if _, err := srv.users.GetBySSOSub("s1"); err == nil {
		t.Fatal("an unauthenticated event provisioned an account")
	}

	malformed := []struct {
		name, eventType string
		user            map[string]any
	}{
		{"bare version", "user.created", func() map[string]any {
			u := scimUser("s2", "s2", true)
			u["meta"] = map[string]any{"version": "3"}
			return u
		}()},
		{"externalId mismatch", "user.created", func() map[string]any {
			u := scimUser("s2", "s2", true)
			u["externalId"] = "other"
			return u
		}()},
		{"active without userName", "user.created", scimUser("s2", "", true)},
		{"active deletion", "user.deleted", scimUser("s2", "s2", true)},
		{"unsupported type", "user.exploded", scimUser("s2", "s2", true)},
		{"wrong schema", "user.created", func() map[string]any {
			u := scimUser("s2", "s2", true)
			u["schemas"] = []string{"urn:ietf:params:scim:schemas:core:2.0:Group"}
			return u
		}()},
	}
	for _, tc := range malformed {
		if rec := postDirectory(t, srv, testSyncKey, tc.eventType, "ev-"+tc.name, 1, tc.user); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if _, err := srv.users.GetBySSOSub("s2"); err == nil {
		t.Fatal("a malformed event provisioned an account")
	}

	// A client secret too short for syncauth is skipped, and the pairing
	// secret still works.
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: "https://idp.example", ClientID: "kypost", ClientSecret: "short"}); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-pairing", 1, scimUser("s3", "s3", true)))

	// The bootstrap admin is the only one; the directory cannot remove it,
	// and a demotion keeps the role rather than locking the instance.
	all, err := srv.users.List()
	if err != nil {
		t.Fatal(err)
	}
	var admin users.User
	for _, u := range all {
		if u.Role == users.RoleAdmin {
			admin = u
		}
	}
	if err := srv.users.LinkSSO(admin.ID, "only-admin", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if rec := postDirectory(t, srv, testSyncKey, "user.deleted", "ev-rm", 1, scimUser("only-admin", "admin", false)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deleting the last admin: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if u, _ := srv.users.Get(admin.ID); !u.Active {
		t.Fatal("the last admin was deactivated despite the refusal")
	}
	// A demotion of the last admin is refused too, and the fence stays where
	// it was: once the directory has promoted somebody else, the very same
	// event applies.
	demote := scimUser("only-admin", "admin", true)
	if rec := postDirectory(t, srv, testSyncKey, "user.updated", "ev-demote", 2, demote); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("demoting the last admin: status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if u, _ := srv.users.Get(admin.ID); u.Role != users.RoleAdmin {
		t.Fatal("the last admin lost the role")
	}
	if _, err := srv.users.Create(context.Background(), "second-admin", "second-admin-password-123", users.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if got := directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-demote", 2, demote)); got != "applied" {
		t.Fatalf("re-delivered demotion with a second admin present: status %q, want applied", got)
	}
	if u, _ := srv.users.Get(admin.ID); u.Role != users.RoleUser {
		t.Fatalf("role after the re-delivered demotion = %q, want user", u.Role)
	}
}

// Deactivation revokes the SSO link of an account that has a credential of
// its own, and must not erase the subject while doing it: every later event
// is addressed by that subject, and a subject that no longer resolves gets a
// second, empty account provisioned on the next one.
func TestDirectoryDisableKeepsTheSubjectItRevokes(t *testing.T) {
	srv, idp := setupSSOTestServer(t)
	srv.pairingSecret = testSyncKey
	const sub = "dir-local"
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser(sub, "dir_local", true)))
	created, err := srv.users.GetBySSOSub(sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.SetPassword(context.Background(), created.ID, "a-local-password-123", false, nil); err != nil {
		t.Fatal(err)
	}
	clearMustChangePassword(t, srv, created.ID)
	before, _ := srv.users.List()

	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-2", 2, scimUser(sub, "dir_local", false)))
	disabled, err := srv.users.GetBySSOSub(sub)
	if err != nil {
		t.Fatalf("revocation lost the subject the directory addresses: %v", err)
	}
	if disabled.Active || !disabled.SSOLinkRevoked() {
		t.Fatalf("after disable: active=%v linkRevoked=%v, want inactive with the link revoked", disabled.Active, disabled.SSOLinkRevoked())
	}

	// The rehire brings the same account back. The link stays revoked: the
	// account has its own credential, so the user signs in locally and
	// re-authorizes the link, as after any other credential revocation.
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.updated", "ev-3", 3, scimUser(sub, "dir_local", true)))
	back, err := srv.users.Get(created.ID)
	if err != nil || !back.Active {
		t.Fatalf("after rehire: %+v, %v; want the same account, active", back, err)
	}
	if after, _ := srv.users.List(); len(after) != len(before) {
		t.Fatalf("account count %d -> %d across disable and rehire; a second account was provisioned", len(before), len(after))
	}
	idp.SetClaims(map[string]any{"sub": sub, "preferred_username": "dir_local", "sid": "sid-1"})
	rec := runSSOFlow(t, srv, idp, nil, false)
	if rec.Code != http.StatusForbidden || sessionCookieFrom(rec) != nil {
		t.Fatalf("SSO sign-in on a revoked link: status = %d, want 403 and no cookie", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "link it again") {
		t.Fatalf("refusal does not tell the user to re-link: %s", rec.Body.String())
	}
}

// The route is public. The pairing secret is in memory, so an unsigned flood
// costs one HMAC and never a read of sso.json.
func TestDirectoryAuthenticatesBeforeReadingSettings(t *testing.T) {
	srv := newDirectoryTestServer(t)
	base := srv.ssoStore.Loads()
	if rec := postDirectory(t, srv, "", "user.created", "ev-1", 1, scimUser("s1", "s1", true)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: status = %d, want 401", rec.Code)
	}
	if n := srv.ssoStore.Loads() - base; n != 0 {
		t.Fatalf("an unsigned request read the settings file %d times, want 0", n)
	}
	// A signature under some other key earns the one read the client-secret
	// fallback needs, and no more.
	if rec := postDirectory(t, srv, "wrong-secret-also-32-bytes-long!", "user.created", "ev-1", 1, scimUser("s1", "s1", true)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rec.Code)
	}
	if n := srv.ssoStore.Loads() - base; n != 1 {
		t.Fatalf("a wrongly signed request read the settings file %d times, want 1", n)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-2", 1, scimUser("s1", "s1", true)))
}

func TestDirectoryProvisionsUnderADistinctNameOnCollision(t *testing.T) {
	srv := newDirectoryTestServer(t)
	if _, err := srv.users.Create(context.Background(), "taken", "a-local-password-123", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-1", 1, scimUser("sub-taken", "taken", true)))
	u, err := srv.users.GetBySSOSub("sub-taken")
	if err != nil || u.Username == "taken" {
		t.Fatalf("provisioned %+v, err %v; want a distinct username", u, err)
	}
	if local, _ := srv.users.GetByUsername("taken"); local.SSOSub != "" {
		t.Fatal("the directory event was linked to the local account of the same name")
	}
}
