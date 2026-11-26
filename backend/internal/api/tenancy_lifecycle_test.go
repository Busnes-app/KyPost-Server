package api

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// setOrg writes org's cache row as a good poll would, now.
func setOrg(t *testing.T, db *sql.DB, org, status string, enabled bool, quotas string) {
	t.Helper()
	ctx := context.Background()
	var rev int64
	if err := db.QueryRowContext(ctx, `SELECT coalesce(MAX(revision),0)+1 FROM orgs`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM orgs WHERE id = ?`, org); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.ApplyOrg(ctx, db, suitedir.Org{ID: org, Status: status, Revision: rev, Enabled: enabled, Quotas: json.RawMessage(quotas)}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// agePoll moves org's last good poll back by d.
func agePoll(t *testing.T, db *sql.DB, org string, d time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE orgs SET polled_at = polled_at - ? WHERE id = ?`, int64(d/time.Second), org); err != nil {
		t.Fatal(err)
	}
}

// orgCase is one cache state for org A and what every surface must answer under it.
type orgCase struct {
	name    string
	set     func(t *testing.T, db *sql.DB)
	served  bool
	receive tenancy.Receiving
	// retry: the cache is only stale, so credentials answer "try again later".
	retry bool
}

func orgCases() []orgCase {
	state := func(status string, enabled bool) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) { setOrg(t, db, orgA, status, enabled, `{}`) }
	}
	aged := func(d time.Duration) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) { setOrg(t, db, orgA, "active", true, `{}`); agePoll(t, db, orgA, d) }
	}
	return []orgCase{
		{"active", state("active", true), true, tenancy.ReceiveAccept, false},
		{"suspended", state("suspended", true), false, tenancy.ReceiveTemporary, false},
		{"deletion_scheduled", state("deletion_scheduled", true), false, tenancy.ReceiveUnknown, false},
		{"deleted", state("deleted", true), false, tenancy.ReceiveUnknown, false},
		{"disabled for KyPost", state("active", false), false, tenancy.ReceiveTemporary, false},
		{"not in the cache", func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`DELETE FROM orgs WHERE id = ?`, orgA); err != nil {
				t.Fatal(err)
			}
		}, false, tenancy.ReceiveTemporary, false},
		{"last good poll 14 minutes ago", aged(14 * time.Minute), true, tenancy.ReceiveAccept, false},
		{"last good poll 16 minutes ago (stale)", aged(16 * time.Minute), false, tenancy.ReceiveAccept, true},
		{"stale and suspended", func(t *testing.T, db *sql.DB) {
			setOrg(t, db, orgA, "suspended", true, `{}`)
			agePoll(t, db, orgA, time.Hour)
		}, false, tenancy.ReceiveTemporary, false},
		// Kept credentials work again with nothing re-issued.
		{"active again", state("active", true), true, tenancy.ReceiveAccept, false},
	}
}

// Each org status against each surface (tenancy design "Org status"): session, paired
// device, CardDAV, MCP agent token, sending, a queued native send and receiving. Every
// credential is kept and works again once the org is active.
func TestOrgStatusOnEverySurface(t *testing.T) {
	fx, _ := newOrgFixture(t)
	db := fx.srv.tenancy
	ctx := context.Background()
	deviceID, deviceSecret := pairNativeDevice(t, fx.srv, fx.a.ID, "org-status-device")
	hash, err := users.HashPassword(ctx, "dav-app-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.srv.writeDAVPassword(fx.a.ID, davPasswordFile{Hash: hash, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	_, agentToken := seedAgentToken(t, fx.srv, fx.a.ID, state.AgentScopeRead, time.Now())

	for i, c := range orgCases() {
		t.Run(c.name, func(t *testing.T) {
			c.set(t, db)
			got := map[string]bool{}

			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			authRequestAs(fx.srv, req, fx.a.ID)
			_, ok, err := fx.srv.currentUser(req)
			if err != nil {
				t.Fatal(err)
			}
			got["session"] = ok

			req = httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
			setDeviceHeaders(req, deviceID, deviceSecret)
			_, _, _, ok, retry := fx.srv.deviceAuthSnapshot(req)
			got["device"] = ok
			_, got["device org"], err = fx.srv.deviceOrg(fx.a.ID)
			if (err != nil) != c.retry || c.retry != (retry == retryAfterKDFBusy) {
				t.Errorf("device: retry %v, deviceOrg err %v; want come back later %v", retry, err, c.retry)
			}

			// Distinct source addresses keep the DAV lockout out of the answer.
			dav := davRequest(fx.srv, fx.a.Username, "dav-app-password", fmt.Sprintf("203.0.113.%d:4000", 10+i)).Code
			mcp := rawMCP(t, fx.srv, listToolsRPC, bearerHeader(agentToken)).Code
			got["carddav"] = dav != http.StatusUnauthorized && dav != http.StatusServiceUnavailable
			got["mcp"] = mcp == http.StatusOK
			refused := http.StatusUnauthorized
			if c.retry {
				refused = http.StatusServiceUnavailable
			}
			if !c.served && (dav != refused || mcp != refused) {
				t.Errorf("carddav %d, mcp %d; want %d", dav, mcp, refused)
			}

			_, err = fx.srv.chargeSend(ctx, "org-status-"+strconv.Itoa(i), fx.a.ID, 1, time.Now())
			if err != nil && !errors.Is(err, tenancy.ErrNotServed) {
				t.Fatal(err)
			}
			got["send"] = err == nil
			got["queued send"] = fx.srv.orgGate().Serve(ctx, orgA) == nil
			// A due scheduled send is held: retried with reasonHeld, which it keeps when it
			// returns to Drafts past the retry window.
			u, err := fx.srv.users.Get(fx.a.ID)
			if err != nil {
				t.Fatal(err)
			}
			job := scheduledJob{ID: "org-scheduled-" + strconv.Itoa(i), UserID: u.ID, Job: mailbox.OutboundJob{NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision}}
			next, reason := fx.srv.dispatchScheduled(ctx, job, time.Now)
			got["scheduled send"] = next != state.ScheduledWaiting || reason != reasonHeld

			for surface, ok := range got {
				if ok != c.served {
					t.Errorf("%s: admitted %v, want %v", surface, ok, c.served)
				}
			}
			o, err := tenancy.LookupOrg(ctx, db, orgA)
			if err != nil {
				t.Fatal(err)
			}
			if o.Receiving() != c.receive {
				t.Errorf("receiving %v, want %v", o.Receiving(), c.receive)
			}
			// Org B is untouched by A's status.
			if err := fx.srv.orgServe(ctx, orgB); err != nil {
				t.Errorf("org B: %v", err)
			}
		})
	}
}

// Browser sign-in and device sign-on are refused under every status but a served one,
// with no session and no log line naming the org.
func TestOrgStatusAtSignIn(t *testing.T) {
	srv, idp, db := multiSSOServer(t)
	provisionMember(t, srv, db, "alice", orgA)
	claims := map[string]any{"sub": "alice", "preferred_username": "u_alice", "roles": []string{}, "org_id": orgA}
	for _, c := range orgCases() {
		c.set(t, db)
		rec := signInRecorder(t, srv, idp, claims)
		if got := sessionCookieFrom(rec) != nil; got != c.served || !c.served && rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, session %v; want session %v", c.name, rec.Code, got, c.served)
		}
	}

	dsrv, didp := nativeSignOnServer(t)
	dsrv.pairingSecret = testSyncKey
	ddb := makeMultiTenant(t, dsrv)
	provisionMember(t, dsrv, ddb, "sso-sub-12345", orgA)
	for i, c := range orgCases() {
		c.set(t, ddb)
		didp.SetClaims(deviceClaims(map[string]any{"jti": "jti-status-" + strconv.Itoa(i), "org_id": orgA}))
		rec := httptest.NewRecorder()
		dsrv.handleNativeSignOn(rec, signOnRequest(didp.IDToken()))
		if ok := rec.Code == http.StatusOK; ok != c.served {
			t.Errorf("device sign-on, %s: status %d, want served %v", c.name, rec.Code, c.served)
		}
	}
}

// orgsIdP is KyIdentity's /api/suite/orgs: it lists the orgs changed after since.
type orgsIdP struct {
	*httptest.Server
	mu     sync.Mutex
	orgs   []suitedir.Org
	status int
	sinces []string
	page   int // orgs per page; 0 lists everything on one page
	erased []erasedSubject
}

func newOrgsIdP(t *testing.T) *orgsIdP {
	idp := &orgsIdP{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); r.URL.Path != "/api/suite/orgs" && r.URL.Path != "/api/suite/erased" || !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		idp.mu.Lock()
		defer idp.mu.Unlock()
		if r.URL.Path == "/api/suite/erased" {
			if idp.status != 0 {
				http.Error(w, "down", idp.status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"subjects": append([]erasedSubject{}, idp.erased...)})
			return
		}
		idp.sinces = append(idp.sinces, r.URL.Query().Get("since"))
		if idp.status != 0 {
			http.Error(w, "down", idp.status)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if c := r.URL.Query().Get("cursor"); c != "" {
			since, _ = strconv.ParseInt(c, 10, 64)
		}
		list := []suitedir.Org{}
		for _, o := range idp.orgs {
			if o.Revision > since {
				list = append(list, o)
			}
		}
		slices.SortStableFunc(list, func(a, b suitedir.Org) int { return cmp.Compare(a.Revision, b.Revision) })
		next := ""
		if idp.page > 0 && len(list) > idp.page {
			list = list[:idp.page]
			next = strconv.FormatInt(list[len(list)-1].Revision, 10)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"orgs": list, "next": next})
	}))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *orgsIdP) set(status int, orgs ...suitedir.Org) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.status, idp.orgs = status, orgs
}

func (idp *orgsIdP) lastSince() string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.sinces[len(idp.sinces)-1]
}

func listed(id, status string, rev int64, quotas ...string) suitedir.Org {
	q := `{}`
	if len(quotas) > 0 {
		q = quotas[0]
	}
	return suitedir.Org{ID: id, Status: status, Revision: rev, Enabled: true, Quotas: json.RawMessage(q), HomeRegion: testCell}
}

func pointAtOrgs(t *testing.T, srv *Server, idp *orgsIdP, secret string) {
	t.Helper()
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL, ClientID: "kypost", ClientSecret: secret}); err != nil {
		t.Fatal(err)
	}
}

func orgRow(t *testing.T, db *sql.DB, org string) tenancy.Org {
	t.Helper()
	o, err := tenancy.LookupOrg(context.Background(), db, org)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// The poller applies KyIdentity's answer: a suspension revokes the members' sessions and
// refuses them on the next request; a stale revision, a change after deleted, a failed
// or unauthenticated poll and a malformed or oversize answer change nothing; the cursor
// re-lists the top revision.
func TestOrgPoller(t *testing.T) {
	fx, _ := newOrgFixture(t)
	srv, db, ctx := fx.srv, fx.srv.tenancy, context.Background()
	idp := newOrgsIdP(t)
	pointAtOrgs(t, srv, idp, "suite-secret")
	if _, err := db.Exec(`DELETE FROM orgs`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	idp.set(0, listed(orgA, "active", 5), listed(orgB, "active", 6))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if idp.lastSince() != "0" || !orgRow(t, db, orgA).Served(now) || !orgRow(t, db, orgB).Served(now) {
		t.Fatalf("first poll: since %s, %+v %+v", idp.lastSince(), orgRow(t, db, orgA), orgRow(t, db, orgB))
	}

	// A member's live session, made once, is revoked by the suspension.
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(srv, req, fx.a.ID)
	token := "session-token-" + fx.a.ID
	idp.set(0, listed(orgA, "suspended", 7), listed(orgB, "active", 6))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if idp.lastSince() != "5" {
		t.Errorf("cursor %s, want 5 (top revision minus one)", idp.lastSince())
	}
	if _, ok, _ := srv.lookupSession(ctx, token); ok {
		t.Fatal("a suspended org's session survived the poll")
	}
	if _, ok, err := srv.currentUser(req); ok || err != nil {
		t.Fatalf("suspended member admitted: %v %v", ok, err)
	}
	if !orgRow(t, db, orgB).Served(now) {
		t.Fatal("org B changed")
	}

	// Stale revision: ignored.
	idp.set(0, listed(orgA, "active", 3))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if o := orgRow(t, db, orgA); o.Status != "suspended" || o.Revision != 7 {
		t.Fatalf("a stale revision applied: %+v", o)
	}
	// Deleted is terminal.
	idp.set(0, listed(orgB, "deleted", 8))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	idp.set(0, listed(orgB, "active", 9))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if o := orgRow(t, db, orgB); o.Status != "deleted" {
		t.Fatalf("deleted org resumed: %+v", o)
	}

	// Resume: the member signs in again; the kept session is gone.
	idp.set(0, listed(orgA, "active", 10))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := srv.currentUser(req); ok {
		t.Fatal("a revoked session came back on resume")
	}

	// Failures change nothing, revoke no session and stamp no poll.
	authRequestAs(srv, req, fx.a.ID)
	before := orgRow(t, db, orgA)
	later := now.Add(10 * time.Minute)
	for name, prep := range map[string]func(){
		"KyIdentity down":    func() { idp.set(http.StatusServiceUnavailable) },
		"wrong credentials":  func() { pointAtOrgs(t, srv, idp, "wrong-secret") },
		"malformed status":   func() { idp.set(0, listed(orgA, "paused", 11)) },
		"non-canonical org":  func() { idp.set(0, listed("ORG-A", "suspended", 11)) },
		"oversize quotas":    func() { idp.set(0, listed(orgA, "suspended", 11, `{"x":"`+strings.Repeat("a", 5000)+`"}`)) },
		"no suite directory": func() { _ = srv.ssoStore.Save(sso.SSOSettings{}) },
	} {
		prep()
		if err := srv.pollOrgs(ctx, later); err == nil {
			t.Errorf("%s: poll succeeded", name)
		}
		if o := orgRow(t, db, orgA); o != before {
			t.Errorf("%s: org changed %+v -> %+v", name, before, o)
		}
		if _, ok, _ := srv.lookupSession(ctx, token); !ok {
			t.Fatalf("%s: a served org's session was revoked", name)
		}
		pointAtOrgs(t, srv, idp, "suite-secret")
	}
	// A good poll with nothing new stamps every row.
	idp.set(0, listed(orgA, "active", 10))
	if err := srv.pollOrgs(ctx, later); err != nil {
		t.Fatal(err)
	}
	if o := orgRow(t, db, orgA); !o.PolledAt.Equal(time.Unix(later.Unix(), 0)) {
		t.Fatalf("good poll not stamped: %v", o.PolledAt)
	}
}

// With the poller failing past the stale bound and the staff org absent from the cache,
// staff keep the console; customers are refused (K8).
func TestStaffKeepTheConsoleWithThePollerFailing(t *testing.T) {
	fx, _, staff := staffFixture(t)
	idp := newOrgsIdP(t)
	pointAtOrgs(t, fx.srv, idp, "suite-secret")
	idp.set(http.StatusBadGateway)
	if err := fx.srv.pollOrgs(context.Background(), time.Now()); err == nil {
		t.Fatal("failing poll succeeded")
	}
	agePoll(t, fx.srv.tenancy, orgA, time.Hour)
	agePoll(t, fx.srv.tenancy, orgB, time.Hour)
	if orgRow(t, fx.srv.tenancy, staffOrg).Known {
		t.Fatal("staff org is cached")
	}
	for _, org := range []string{"", orgA} {
		w := fx.staffCall(staff, http.MethodGet, "/api/admin/mailboxes", "", org)
		if org == "" && w.Code != http.StatusBadRequest || org != "" && w.Code != http.StatusOK {
			t.Errorf("staff console (org %q): %d %s", org, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(fx.srv, req, fx.a.ID)
	if _, ok, _ := fx.srv.currentUser(req); ok {
		t.Fatal("a customer kept a session on a stale cache")
	}
}

// Each quota from quotas_json: the org send caps (summed over members, refusing without
// charging), the mailboxes cap, and the parse rules. A lowered quota keeps what exists.
func TestOrgQuotas(t *testing.T) {
	fx, _, staff := staffFixture(t)
	srv, db, ctx := fx.srv, fx.srv.tenancy, context.Background()
	now := time.Now()
	charge := func(m orgMember, id string, recipients int) error {
		_, err := srv.chargeSend(ctx, id, m.ID, recipients, now)
		return err
	}

	setOrg(t, db, orgA, "active", true, `{"sendPerHour":2}`)
	if err := charge(fx.a, "q1", 1); err != nil {
		t.Fatal(err)
	}
	if err := charge(fx.other, "q2", 1); err != nil {
		t.Fatal(err)
	}
	if err := charge(fx.a, "q3", 1); !errors.Is(err, state.ErrSendQuota) {
		t.Fatalf("third org message in an hour: %v", err)
	}
	if err := charge(fx.b, "q4", 1); err != nil {
		t.Fatalf("org B is not capped by A: %v", err)
	}
	if m, _, err := srv.globalStore.OrgSendUsage(ctx, []string{fx.a.ID, fx.other.ID}, now.Unix()); err != nil || m != 2 {
		t.Fatalf("a refused send was charged: %d %v", m, err)
	}
	// A retry of an admitted send is admitted again without a second charge.
	if err := charge(fx.a, "q1", 1); err != nil {
		t.Fatalf("retry of an admitted send: %v", err)
	}
	// A retry refused by a lowered cap keeps its earlier charge.
	setOrg(t, db, orgA, "active", true, `{"sendPerHour":1}`)
	if err := charge(fx.a, "q1", 1); !errors.Is(err, state.ErrSendQuota) {
		t.Fatalf("retry past a lowered cap: %v", err)
	}
	if m, _, err := srv.globalStore.OrgSendUsage(ctx, []string{fx.a.ID, fx.other.ID}, now.Unix()); err != nil || m != 2 {
		t.Fatalf("a refused retry released its earlier charge: %d %v", m, err)
	}

	setOrg(t, db, orgA, "active", true, `{"recipientsPerDay":5}`)
	// q1 and q2 hold 2 of org A's recipients today.
	if err := charge(fx.other, "r1", 4); !errors.Is(err, state.ErrSendQuota) {
		t.Fatalf("org recipients past the day cap: %v", err)
	}
	if err := charge(fx.other, "r2", 3); err != nil {
		t.Fatal(err)
	}

	// Malformed and negative quotas read as zero: nothing new is sent.
	for _, q := range []string{`{"sendPerHour":-1}`, `{"sendPerHour":"10"}`, `{"sendPerHour":1.5}`} {
		setOrg(t, db, orgA, "active", true, q)
		if err := charge(fx.a, "bad-"+q, 1); !errors.Is(err, state.ErrSendQuota) {
			t.Errorf("%s: %v", q, err)
		}
	}

	// mailboxes: org A holds two extra mailboxes (one per member).
	createExtra := func(m orgMember, addr string) *httptest.ResponseRecorder {
		return fx.staffCall(staff, http.MethodPost, "/api/admin/mailboxes", `{"user":"`+m.ID+`","address":"`+addr+`"}`, orgA)
	}
	setOrg(t, db, orgA, "active", true, `{"mailboxes":2}`)
	if w := createExtra(fx.a, "third-a@example.test"); w.Code != http.StatusConflict {
		t.Fatalf("extra mailbox past the quota: %d %s", w.Code, w.Body.String())
	}
	setOrg(t, db, orgA, "active", true, `{"mailboxes":3}`)
	if w := createExtra(fx.a, "third-a@example.test"); w.Code != http.StatusOK {
		t.Fatalf("extra mailbox within the quota: %d %s", w.Code, w.Body.String())
	}
	// Lowering the quota keeps all three.
	setOrg(t, db, orgA, "active", true, `{"mailboxes":1}`)
	mailboxes, err := srv.ssoLifecycle.NativeMailboxes()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range mailboxes {
		if m.Kind == "extra" && (m.User == fx.a.ID || m.User == fx.other.ID) {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("org A has %d extra mailboxes after lowering the quota, want 3", n)
	}
}

// Single mode: no poll, no gate, no org cap, and chargeSend unchanged.
func TestSingleTenantHasNoLifecycle(t *testing.T) {
	srv := newTestServer(t)
	idp := newOrgsIdP(t)
	pointAtOrgs(t, srv, idp, "suite-secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.StartOrgPoller(ctx)
	if len(idp.sinces) != 0 {
		t.Fatal("a single-tenant install polled /api/suite/orgs")
	}
	if err := srv.orgServe(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	u := srv.mustBootstrapUserID(t)
	if _, err := srv.chargeSend(context.Background(), "single-1", u, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Two replicas on one PostgreSQL see one suspension: the poll on A refuses the member's
// session and sends on B at the next request (tenancy design "Replicas").
func TestTwoReplicasSeeOneSuspension(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, _ := setupSSOTestServer(t)
	a.pairingSecret = testSyncKey
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbA, err := tenancy.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbA.Close() })
	a.tenancy, a.staffOrg, a.users.StaffOrg = dbA, staffOrg, staffOrg
	setOrg(t, dbA, orgA, "active", true, `{}`)
	member := provisionMember(t, a, dbA, "replica-member", orgA)

	b := replicaOf(t, a)
	dbB, err := tenancy.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbB.Close() })
	b.tenancy, b.staffOrg, b.users.StaffOrg = dbB, staffOrg, staffOrg
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(a, req, member.ID)
	if _, ok, err := b.currentUser(req); !ok || err != nil {
		t.Fatalf("B before the suspension: %v %v", ok, err)
	}
	idp := newOrgsIdP(t)
	pointAtOrgs(t, a, idp, "suite-secret")
	idp.set(0, listed(orgA, "suspended", 2))
	if err := a.pollOrgs(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := b.currentUser(req); ok || err != nil {
		t.Fatalf("B after the suspension on A: %v %v", ok, err)
	}
	if _, err := b.chargeSend(context.Background(), "replica-send", member.ID, 1, time.Now()); !errors.Is(err, tenancy.ErrNotServed) {
		t.Fatalf("B sent for a suspended org: %v", err)
	}
	if _, ok, _ := b.lookupSession(context.Background(), "session-token-"+member.ID); ok {
		t.Fatal("the shared session survived the suspension")
	}
}

// An unreadable org cache is "try again later" on every credential path, never a dead
// credential: a 401 would make a CardDAV client drop its password and a phone re-pair.
func TestOrgCacheOutageIsRetryable(t *testing.T) {
	fx, _ := newOrgFixture(t)
	ctx := context.Background()
	deviceID, deviceSecret := pairNativeDevice(t, fx.srv, fx.a.ID, "outage-device")
	hash, err := users.HashPassword(ctx, "dav-app-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.srv.writeDAVPassword(fx.a.ID, davPasswordFile{Hash: hash, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	_, agentToken := seedAgentToken(t, fx.srv, fx.a.ID, state.AgentScopeRead, time.Now())
	if err := fx.srv.tenancy.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	authRequestAs(fx.srv, req, fx.a.ID)
	if _, ok, err := fx.srv.currentUser(req); ok || err == nil {
		t.Errorf("session: ok %v err %v, want a retryable error", ok, err)
	}
	if _, ok, err := fx.srv.currentUser(req); err == nil || ok {
		t.Error("the session was dropped by the outage")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/contacts", nil)
	setDeviceHeaders(req, deviceID, deviceSecret)
	if _, _, _, ok, retry := fx.srv.deviceAuthSnapshot(req); ok || retry != retryAfterKDFBusy {
		t.Errorf("device: ok %v retry %v, want come back later", ok, retry)
	}
	if _, _, err := fx.srv.deviceOrg(fx.a.ID); err == nil {
		t.Error("deviceOrg hid the outage")
	}
	if w := davRequest(fx.srv, fx.a.Username, "dav-app-password", "203.0.113.90:4000"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("carddav: %d, want 503", w.Code)
	}
	// A wrong password still gets 401: the outage is reported only after the password.
	if w := davRequest(fx.srv, fx.a.Username, "wrong", "203.0.113.91:4000"); w.Code != http.StatusUnauthorized {
		t.Errorf("carddav wrong password: %d, want 401", w.Code)
	}
	if w := rawMCP(t, fx.srv, listToolsRPC, bearerHeader(agentToken)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("mcp: %d, want 503", w.Code)
	}
}

// A backlog past one Orgs call's page bound (100 pages) is read to the end before
// MarkPolled, so a suspension on page 101 is not hidden behind a fresh stamp.
func TestOrgPollerReadsPastThePageBound(t *testing.T) {
	fx, _ := newOrgFixture(t)
	srv, db, ctx := fx.srv, fx.srv.tenancy, context.Background()
	idp := newOrgsIdP(t)
	pointAtOrgs(t, srv, idp, "suite-secret")
	if _, err := db.Exec(`DELETE FROM orgs`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	idp.set(0, listed(orgA, "active", 1))
	if err := srv.pollOrgs(ctx, now); err != nil {
		t.Fatal(err)
	}
	backlog := []suitedir.Org{}
	for i := range 100 {
		backlog = append(backlog, listed(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "active", int64(i+2)))
	}
	idp.set(0, append(backlog, listed(orgA, "suspended", 102))...)
	idp.mu.Lock()
	idp.page = 1
	idp.mu.Unlock()
	if err := srv.pollOrgs(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if orgRow(t, db, orgA).Served(now.Add(time.Minute)) {
		t.Fatal("a suspension past the page bound was stamped fresh and left served")
	}
}
