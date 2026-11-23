//go:build linux

package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/groups"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// TestCrossOrgMatrix (tenancy design, "Proving a cross-org ID answers exactly like a
// missing one"; build plan PR 3, grown by later PRs). Every org-bound route is called as
// a member of org A three times: with A's own object (the control), with org B's object,
// and with an ID that never existed. B's answer must equal the missing one byte for byte,
// B's stores must not change, and no log line may name B. It runs on SQLite and, in the
// postgres job, on PostgreSQL.
//
// Org-bound here: every route wrapped in withMailbox (the X-KyPost-Mailbox hop), every
// route that admits a mailbox from its body, and every non-administrator route with a
// path parameter. Administrator routes are PR 4's staff variant. A route needs a case in
// crossOrgCases or a reason in crossOrgNotBound, or the build fails.

type crossOrgRoute struct {
	method, pattern string
	mailbox         bool // wrapped in withMailbox
	admin           bool
	class           string // the admin route class, adminRouteClasses
}

// crossOrgRoutes parses server.go's route table.
func crossOrgRoutes(t *testing.T) []crossOrgRoute {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []crossOrgRoute
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			return true
		}
		r := crossOrgRoute{method: method, pattern: path}
		ast.Inspect(call.Args[1], func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				r.mailbox = r.mailbox || s.Sel.Name == "withMailbox"
				r.admin = r.admin || adminRouteClasses[s.Sel.Name] != "" || s.Sel.Name == "withAdmin"
				r.class = cmp.Or(r.class, adminRouteClasses[s.Sel.Name])
			}
			return true
		})
		out = append(out, r)
		return true
	})
	return out
}

// bodyMailboxRoutes admit a mailbox named in the JSON body (admitMailbox(…, body.Mailbox)).
var bodyMailboxRoutes = map[string]bool{"POST /api/export": true, "POST /api/import": true, "POST /api/import/imap": true}

func (r crossOrgRoute) key() string { return r.method + " " + r.pattern }

func (r crossOrgRoute) orgBound() bool {
	return r.mailbox || bodyMailboxRoutes[r.key()] || !r.admin && strings.Contains(r.pattern, "{")
}

// crossOrgNotBound are routes with a path parameter that names no org-owned object.
var crossOrgNotBound = map[string]string{
	"PUT /api/pgp/identity/envelope/{slot}":    "slot is a fixed slot name in the caller's own key envelope, not an object ID",
	"GET /api/pgp/identity/envelope/{slot}":    "slot is a fixed slot name in the caller's own key envelope, not an object ID",
	"DELETE /api/pgp/identity/envelope/{slot}": "slot is a fixed slot name in the caller's own key envelope, not an object ID",
	"GET /pickup/{id}":                         "a signed public pickup link anyone holding it may open, by design; no session and no org (PR 5 answers a suspended owner's link as expired)",
	"POST /pickup/{id}/open":                   "a signed public pickup link anyone holding it may open, by design; no session and no org (PR 5 answers a suspended owner's link as expired)",
	"POST /pickup/{id}/blob":                   "a signed public pickup link anyone holding it may open, by design; no session and no org (PR 5 answers a suspended owner's link as expired)",
}

// orgMember is one fixture account.
type orgMember struct {
	users.User
	extra string // an extra mailbox it owns
}

type orgFixture struct {
	t     *testing.T
	srv   *Server
	a, b  orgMember
	other orgMember // a second member of A, for the control of hops between users
}

// call sends one request as m and returns the recorder.
func (fx *orgFixture) call(m orgMember, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	authRequestAs(fx.srv, req, m.ID)
	req.Header.Set("Origin", "http://example.com")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	fx.srv.routes().ServeHTTP(w, req)
	return w
}

// created posts body to path as m and returns the JSON field named field.
func (fx *orgFixture) created(m orgMember, path, body, field string) string {
	fx.t.Helper()
	w := fx.call(m, http.MethodPost, path, body, nil)
	var out map[string]any
	if w.Code >= 300 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		fx.t.Fatalf("seed %s: %d %s", path, w.Code, w.Body)
	}
	id, _ := out[field].(string)
	if id == "" {
		fx.t.Fatalf("seed %s: no %q in %s", path, field, w.Body)
	}
	return id
}

// crossOrgCase builds one route's request for an object, and seeds that object for a member.
type crossOrgCase struct {
	// seed creates the object for m and returns its ID; nil means the route's own
	// object is m's extra mailbox.
	seed func(fx *orgFixture, m orgMember) string
	// missing is an ID that never existed, in the form seed returns.
	missing string
	// body is the JSON body, with {id} replaced; header names a header that carries {id}.
	body, header string
	// control is the status the caller's own object gets: 0 means any status below 300,
	// -1 only that it answers unlike a missing object, and -2 that the route confirms the
	// caller's credential (a live step-up) before it reads the object, so no control can
	// be reached here and B's object and a missing one are compared only.
	control int
}

func (c crossOrgCase) request(fx *orgFixture, r crossOrgRoute, id string) *httptest.ResponseRecorder {
	path := r.pattern
	if i := strings.Index(path, "{"); i >= 0 {
		j := strings.Index(path[i:], "}")
		path = path[:i] + id + path[i+j+1:]
	}
	// A refused credential confirmation strikes the caller's step-up lockout; clear it so
	// the next call is answered on its own merits.
	fx.srv.passwordChangeLockout.recordSuccess(stepUpLockoutKey(fx.a.ID, httptest.NewRequest(http.MethodGet, "/", nil)))
	var header map[string]string
	if c.header != "" {
		header = map[string]string{c.header: id}
	}
	return fx.call(fx.a, r.method, path, strings.ReplaceAll(c.body, "{id}", id), header)
}

// mailboxHeaderCase is every withMailbox route: the header names another org's mailbox.
var mailboxHeaderCase = crossOrgCase{header: mailboxHeader, missing: "mbx-00000000-0000-4000-8000-000000000000", control: -1}

func extraOf(_ *orgFixture, m orgMember) string { return m.extra }

const missingUUID = "00000000-0000-4000-8000-000000000000"

// message imports one message into m's primary mailbox and returns its wire ID.
func (fx *orgFixture) message(m orgMember) string {
	ctx := context.Background()
	a, err := fx.srv.ssoLifecycle.AdmitNativeMailbox(ctx, fx.srv.stateDir, "https://idp.example", m.ID, m.ID, fx.srv.users)
	check(fx, err)
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(fx.srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	check(fx, err)
	defer store.Close()
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{a.Address}, Subject: "seed", Body: "seed body"}.Build()
	id, err := store.Import(ctx, mailbox.Receipt{Gateway: "qualified-test", Delivery: newID(fx), Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: a.Address, Generation: 1}}}, bytes.NewReader(raw))
	check(fx, err)
	return "n1:" + store.MessageReferenceGeneration() + ":" + strconv.FormatInt(id, 10)
}

// check fails the seed on err.
func check(fx *orgFixture, err error) {
	fx.t.Helper()
	if err != nil {
		fx.t.Fatal(err)
	}
}

func newID(fx *orgFixture) string {
	id, err := fsutil.NewUUIDv4()
	check(fx, err)
	return id
}

func seedContact(fx *orgFixture, m orgMember) string {
	store, err := fx.srv.userContactsStore(m.ID)
	check(fx, err)
	c, err := store.Upsert(contacts.Contact{FormattedName: "Seed " + m.Username, Emails: []contacts.ContactValue{{Value: "seed@outside.test"}}})
	check(fx, err)
	return c.UID
}

func seedGroup(fx *orgFixture, m orgMember) string {
	store, err := fx.srv.userGroupsStore(m.ID)
	check(fx, err)
	g, err := store.Upsert(groups.Group{Name: "seed"})
	check(fx, err)
	return g.ID
}

func seedRule(fx *orgFixture, m orgMember) string {
	return fx.created(m, "/api/rules", `{"name":"seed","enabled":false,"match":{"op":"all","conditions":[{"field":"from","op":"contains","value":"x"}]},"actions":[{"type":"read"}]}`, "id")
}

func seedSendAs(fx *orgFixture, m orgMember) string {
	store, err := fx.srv.userSendAsStore(m.ID)
	check(fx, err)
	a, err := store.Create(m.ID, "seed-"+m.Username+"@outside.test", "Seed")
	check(fx, err)
	return a.ID
}

func seedOrgAgentToken(fx *orgFixture, m orgMember) string {
	id, _ := seedAgentToken(fx.t, fx.srv, m.ID, state.AgentScopeRead, time.Now())
	return id
}

func seedSuppression(fx *orgFixture, m orgMember) string {
	email := "seed-" + m.Username + "@outside.test"
	if err := pgpdiscovery.AddSuppression(fx.srv.userStateDir(m.ID), email, pgpdiscovery.ReasonExplicit); err != nil {
		fx.t.Fatal(err)
	}
	return email
}

func seedDevice(fx *orgFixture, m orgMember) string {
	id := "dev-" + newID(fx)
	now := time.Now().UTC().Format(time.RFC3339)
	store, err := fx.srv.userStore(m.ID)
	check(fx, err)
	if err := store.UpsertNativeDevice(state.NativeDevice{DeviceID: id, Platform: "android", PushToken: "seed", RegisteredAt: now, UpdatedAt: now, UserID: m.ID}); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func seedPending(fx *orgFixture, m orgMember) string {
	id := "dev-" + newID(fx)
	now := time.Now().UTC()
	fx.srv.pairingMu.Lock()
	err := fx.srv.pendingPut(pendingDevice{OwnerID: m.ID, Device: state.NativeDevice{DeviceID: id, Platform: "android", UserID: m.ID}, ApprovalCode: "123456", RequestedAt: now, ExpiresAt: now.Add(5 * time.Minute)})
	fx.srv.pairingMu.Unlock()
	if err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func seedFlag(fx *orgFixture, m orgMember) string {
	ref, err := kycalendar.OpenFlags(fx.srv.userStateDir(m.ID)).Put(kycalendar.Flag{Mailbox: m.ID, Folder: "INBOX", Message: fx.message(m)})
	check(fx, err)
	return ref
}

func seedMasked(fx *orgFixture, m orgMember) string {
	return fx.created(m, "/api/addresses/masked", `{"domain":"example.test"}`, "address")
}

func seedStepUp(fx *orgFixture, m orgMember) string {
	id := newID(fx)
	if err := fx.srv.putStepUp(id, ssoStepUp{Session: sessionHash("session-token-" + m.ID), Action: "seed", Created: time.Now(), Expires: time.Now().Add(5 * time.Minute)}); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func seedExport(fx *orgFixture, m orgMember) string {
	id := newID(fx)
	g := exportGrant{User: m.ID, Session: sessionHash("session-token-" + m.ID), Mailbox: m.ID, Format: "mbox", Correlation: id, Expires: time.Now().Add(5 * time.Minute)}
	if err := fx.srv.putExportGrant(id, g); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func seedImport(imap bool) func(*orgFixture, orgMember) string {
	return func(fx *orgFixture, m orgMember) string {
		id := newID(fx)
		g := importGrant{User: m.ID, Session: sessionHash("session-token-" + m.ID), Mailbox: m.ID, Correlation: id, Expires: time.Now().Add(5 * time.Minute)}
		if imap {
			g.IMAP = &imapGrant{Host: "imap.outside.test", Port: 993, Username: "seed"}
		}
		if err := fx.srv.putImportGrant(id, g); err != nil {
			fx.t.Fatal(err)
		}
		return id
	}
}

// Cases whose own-object control cannot succeed without a step-up or a live peer use
// control -1: the caller's own object must at least answer unlike a missing one.
var crossOrgCases = map[string]crossOrgCase{
	"POST /api/export":      {seed: extraOf, missing: mailboxHeaderCase.missing, body: `{"mailbox":"{id}","format":"mbox"}`, control: -1},
	"POST /api/import":      {seed: extraOf, missing: mailboxHeaderCase.missing, body: `{"mailbox":"{id}"}`, control: -1},
	"POST /api/import/imap": {seed: extraOf, missing: mailboxHeaderCase.missing, body: `{"mailbox":"{id}"}`, control: -1},

	"GET /api/auth/oidc/step-up/{id}":                           {seed: seedStepUp, missing: missingUUID},
	"DELETE /api/auth/oidc/step-up/{id}":                        {seed: seedStepUp, missing: missingUUID},
	"GET /api/mail/flags/{ref}":                                 {seed: seedFlag, missing: missingUUID},
	"PUT /api/addresses/masked/{address}":                       {seed: seedMasked, missing: "never-issued@example.test", body: `{"note":"x"}`},
	"DELETE /api/addresses/masked/{address}":                    {seed: seedMasked, missing: "never-issued@example.test"},
	"GET /api/export/{token}":                                   {seed: seedExport, missing: missingUUID, control: -1},
	"POST /api/import/{token}":                                  {seed: seedImport(false), missing: missingUUID, control: -1},
	"POST /api/import/imap/{token}/folders":                     {seed: seedImport(true), missing: missingUUID, body: `{"password":"x"}`, control: -1},
	"POST /api/import/imap/{token}/start":                       {seed: seedImport(true), missing: missingUUID, body: `{"password":"x"}`, control: -1},
	"GET /api/mail/outbox/{id}":                                 {seed: extraOf, header: mailboxHeader, missing: mailboxHeaderCase.missing, control: -1},
	"POST /api/scheduled/{id}/cancel":                           {seed: extraOf, header: mailboxHeader, missing: mailboxHeaderCase.missing, control: -1},
	"DELETE /api/mail/send-as/{id}":                             {seed: seedSendAs, missing: missingUUID},
	"POST /api/mail/send-as/{id}/confirm":                       {seed: seedSendAs, missing: missingUUID, body: `{"code":"000000"}`, control: -1},
	"PUT /api/agent-tokens/{id}":                                {seed: seedOrgAgentToken, missing: missingUUID, body: `{"scope":"mcp:organize"}`, control: -1},
	"DELETE /api/agent-tokens/{id}":                             {seed: seedOrgAgentToken, missing: missingUUID},
	"GET /api/contacts/{id}":                                    {seed: seedContact, missing: missingUUID},
	"PUT /api/contacts/{id}":                                    {seed: seedContact, missing: missingUUID, body: `{"fn":"Changed"}`},
	"DELETE /api/contacts/{id}":                                 {seed: seedContact, missing: missingUUID},
	"POST /api/contacts/{id}/photo":                             {seed: seedContact, missing: missingUUID, control: -1},
	"GET /api/contacts/{id}/photo":                              {seed: seedContact, missing: missingUUID, control: -1},
	"DELETE /api/contacts/{id}/photo":                           {seed: seedContact, missing: missingUUID, control: -1},
	"POST /api/contacts/{id}/self":                              {seed: seedContact, missing: missingUUID, body: `{"self":true}`},
	"POST /api/contacts/{id}/pgp-key/confirm":                   {seed: seedContact, missing: missingUUID, body: `{"fingerprint":"0000000000000000000000000000000000000000"}`, control: -2},
	"PUT /api/groups/{id}":                                      {seed: seedGroup, missing: missingUUID, body: `{"name":"changed"}`},
	"DELETE /api/groups/{id}":                                   {seed: seedGroup, missing: missingUUID},
	"DELETE /api/pgp/discovery/suppressions/{email}":            {seed: seedSuppression, missing: "never@outside.test"},
	"PUT /api/notifications/native/devices/{deviceId}/mfa":      {seed: seedDevice, missing: "dev-" + missingUUID, body: `{"approver":true}`, control: -2},
	"POST /api/notifications/native/pending/{deviceId}/approve": {seed: seedPending, missing: "dev-" + missingUUID, body: `{}`, control: -1},
	"POST /api/notifications/native/pending/{deviceId}/deny":    {seed: seedPending, missing: "dev-" + missingUUID, body: `{}`},
	"PUT /api/rules/{id}":                                       {seed: seedRule, missing: missingUUID, body: `{"name":"changed","enabled":false,"match":{"op":"all","conditions":[{"field":"from","op":"contains","value":"x"}]},"actions":[{"type":"read"}]}`},
	"DELETE /api/rules/{id}":                                    {seed: seedRule, missing: missingUUID},
	"GET /api/rules/{id}/sieve":                                 {seed: seedRule, missing: missingUUID, control: -1},
	"PUT /api/rules/{id}/sieve":                                 {seed: seedRule, missing: missingUUID, body: `{"sieve":"keep;"}`, control: -1},
}

// normalized is a response with the volatile headers removed and the ID used replaced, so
// an answer that echoes the ID it was given still compares equal.
func normalized(w *httptest.ResponseRecorder, id string) string {
	h := w.Header().Clone()
	h.Del("Date")
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(strconv.Itoa(w.Code) + "\n")
	for _, k := range keys {
		b.WriteString(k + ": " + strings.Join(h[k], ",") + "\n")
	}
	b.WriteString(strings.ReplaceAll(w.Body.String(), id, "<id>"))
	return b.String()
}

// orgDigest hashes every file under m's user and mailbox directories (on PostgreSQL the
// SQLite stores are schemas, so view adds what m itself reads back) and m's record.
func (fx *orgFixture) orgDigest(m orgMember) string {
	fx.t.Helper()
	var parts []string
	sum := func(name string, b []byte) {
		h := sha256.Sum256(b)
		parts = append(parts, name+"="+hex.EncodeToString(h[:8]))
	}
	// m's own reads first: they open stores, which may create files.
	for _, path := range []string{"/api/contacts", "/api/groups", "/api/rules", "/api/mail/send-as", "/api/agent-tokens", "/api/addresses/masked", "/api/pgp/discovery/suppressions", "/api/notifications/native/devices"} {
		w := fx.call(m, http.MethodGet, path, "", nil)
		sum(path, append([]byte(strconv.Itoa(w.Code)), w.Body.Bytes()...))
	}
	for _, dir := range []string{fx.srv.userConfigDir(m.ID), fx.srv.userStateDir(m.ID), filepath.Join(fx.srv.stateDir, "mailboxes", m.extra)} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "-shm") {
				return nil
			}
			raw, _ := os.ReadFile(p)
			sum(p, raw)
			return nil
		})
	}
	// UpdatedAt moves on every authRequestAs (it clears MustChangePassword).
	u, _ := fx.srv.users.Get(m.ID)
	u.UpdatedAt = ""
	rec, _ := json.Marshal(u)
	sum("users.json", rec)
	return strings.Join(parts, "\n")
}

// orgDigestDiff names the parts of two digests that differ.
func orgDigestDiff(before, after string) string {
	var out []string
	b := strings.Split(before, "\n")
	for _, p := range strings.Split(after, "\n") {
		if !slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// syncBuffer is a log sink the server writes to from request goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

func newOrgFixture(t *testing.T) (*orgFixture, *syncBuffer) {
	t.Helper()
	t.Setenv("SECRET_DIR", t.TempDir())
	srv := newNativeRuntimeServer(t)
	makeMultiTenant(t, srv)
	logs := &syncBuffer{}
	logger, err := logging.NewWithOutput(logs)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = logger
	// The matrix makes many writes as one account; the meter has its own tests.
	srv.accountWriteLimiter = newKeyedLimiter(1<<20, 1<<20)
	if err := srv.globalStore.SetSetting(maskedDomainsSetting, "example.test"); err != nil {
		t.Fatal(err)
	}
	fx := &orgFixture{t: t, srv: srv}
	member := func(sub, org string) orgMember {
		u := orgUser(sub, org, true)
		directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-"+sub, 1, u))
		rec, err := srv.users.GetBySSOSub(sub)
		if err != nil || rec.OrgID != org {
			t.Fatalf("%s: %+v %v", sub, rec, err)
		}
		box, err := srv.ssoLifecycle.CreateNativeMailbox(context.Background(), srv.stateDir, rec.ID, sub+"-box@example.test")
		if err != nil {
			t.Fatal(err)
		}
		return orgMember{User: rec, extra: box.ID}
	}
	fx.a, fx.b, fx.other = member("member-a", orgA), member("member-b", orgB), member("member-a2", orgA)
	return fx, logs
}

func TestCrossOrgMatrix(t *testing.T) {
	fx, logs := newOrgFixture(t)
	runCrossOrgMatrix(t, fx, logs)
}

func runCrossOrgMatrix(t *testing.T, fx *orgFixture, logs *syncBuffer) {
	routes := crossOrgRoutes(t)
	covered := 0
	for _, r := range routes {
		if !r.orgBound() {
			continue
		}
		c, ok := crossOrgCases[r.key()]
		switch {
		case ok:
		case crossOrgNotBound[r.key()] != "":
			continue
		case r.mailbox:
			c = mailboxHeaderCase
		default:
			t.Errorf("org-bound route %q has no case in crossOrgCases and no reason in crossOrgNotBound", r.key())
			continue
		}
		covered++
		t.Run(r.key(), func(t *testing.T) {
			fx.t = t
			seed := c.seed
			if seed == nil {
				seed = extraOf
			}
			own, foreign := seed(fx, fx.a), seed(fx, fx.b)
			logs.take()
			if w := c.request(fx, r, own); c.control == 0 && w.Code >= 300 || c.control > 0 && w.Code != c.control {
				t.Errorf("control with A's own object: %d %s", w.Code, w.Body)
			} else if c.control == -1 && normalized(w, own) == normalized(c.request(fx, r, c.missing), c.missing) {
				t.Errorf("control with A's own object answered like a missing one: %d %s", w.Code, w.Body)
			}
			before := fx.orgDigest(fx.b)
			logs.take()
			gotB := normalized(c.request(fx, r, foreign), foreign)
			lines := logs.take()
			gotMissing := normalized(c.request(fx, r, c.missing), c.missing)
			if gotB != gotMissing {
				t.Errorf("B's object answered unlike a missing one:\nB:       %q\nmissing: %q", gotB, gotMissing)
			}
			if after := fx.orgDigest(fx.b); after != before {
				t.Errorf("the call with B's object changed B's stores: %s", orgDigestDiff(before, after))
			}
			for _, name := range []string{fx.b.ID, fx.b.SSOSub, fx.b.extra, fx.b.Username} {
				if strings.Contains(lines, name) {
					t.Errorf("a log line names B (%q): %s", name, lines)
				}
			}
		})
	}
	if covered == 0 {
		t.Fatal("no org-bound route found")
	}
}
