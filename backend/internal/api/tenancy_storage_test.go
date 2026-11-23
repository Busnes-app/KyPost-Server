package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// storageIdP is KyIdentity's POST /api/suite/storage (KyIdentity tenancy spec "Org storage
// pool"): it records each report and answers with room(org, used) more bytes as an ok
// grant, or with result for every org when result is set.
type storageIdP struct {
	*httptest.Server
	mu      sync.Mutex
	reports map[string]tenancy.StorageReport
	status  int
	result  string
	room    func(org string, used int64) int64
	ttl     int64
}

func newStorageIdP(t *testing.T) *storageIdP {
	idp := &storageIdP{reports: map[string]tenancy.StorageReport{}, room: func(string, int64) int64 { return 1 << 30 }, ttl: 900}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); r.Method != http.MethodPost || r.URL.Path != "/api/suite/storage" || !ok || id != "kypost" || secret != "suite-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		var body struct {
			Reports []tenancy.StorageReport `json:"reports"`
		}
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &body) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		defer idp.mu.Unlock()
		if idp.status != 0 {
			http.Error(w, "down", idp.status)
			return
		}
		results := []tenancy.StorageResult{}
		for _, rep := range body.Reports {
			idp.reports[rep.OrgID] = rep
			res := tenancy.StorageResult{OrgID: rep.OrgID, Result: "ok", Seq: rep.Seq, CeilingBytes: rep.UsedBytes + idp.room(rep.OrgID, rep.UsedBytes), TTLSeconds: idp.ttl}
			if idp.result != "" {
				res.Result = idp.result
			}
			results = append(results, res)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *storageIdP) set(f func(idp *storageIdP)) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	f(idp)
}

func (idp *storageIdP) report(org string) (tenancy.StorageReport, bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	r, ok := idp.reports[org]
	return r, ok
}

// openMailbox opens m's primary mailbox store.
func (fx *orgFixture) openMailbox(m orgMember) *mailbox.Store {
	fx.t.Helper()
	a, err := fx.srv.ssoLifecycle.AdmitNativeMailbox(context.Background(), fx.srv.stateDir, "https://idp.example", m.ID, m.ID, fx.srv.users)
	check(fx, err)
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(fx.srv.stateDir), "mailbox"), a.Owner, a.Limits, a.Source)
	check(fx, err)
	fx.t.Cleanup(func() { _ = store.Close() })
	return store
}

var poolDraft = mailmsg.Message{From: "a@example.test", To: []string{"x@outside.test"}, Subject: "draft", Body: "draft body"}.Build()

// The org storage pool, KyPost's side (tenancy design "Quotas"): KyPost reports each org's
// own bytes (mail in every mailbox of every member, and contact photos), never another
// org's or the staff org's; checks every write that adds bytes against the grant, exactly
// to the byte; fails closed with no grant, an expired one, an unknown org or the grant
// zeroed for an org over its pool; keeps the grant through a failed or stale report; and
// still imports mail receiving accepted. A deletion frees room at once.
func TestOrgStoragePool(t *testing.T) {
	fx, _ := newOrgFixture(t)
	srv, db, ctx := fx.srv, fx.srv.tenancy, context.Background()
	setOrg(t, db, staffOrg, "active", true, `{}`)
	mailbox.SetPoolCheck(srv.StoragePoolCheck)
	t.Cleanup(func() { mailbox.SetPoolCheck(nil) })
	boxA, boxB := fx.openMailbox(fx.a), fx.openMailbox(fx.b)
	draft := func(box *mailbox.Store) error {
		_, err := box.Append(ctx, "Drafts", bytes.NewReader(poolDraft), true)
		return err
	}

	// No grant yet: nothing new is stored, but mail receiving accepted still imports.
	if err := draft(boxA); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a write before any grant: %v", err)
	}
	// A send is refused at enqueue, naming the pool, before anything is queued or sent.
	_, err := mailmsg.SaveDomainRelay(ctx, filepath.Join(srv.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"), mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: 1, Username: "u", Password: "p"})
	check(fx, err)
	if w := fx.call(fx.a, http.MethodPost, "/api/mail/send", `{"to":"x@outside.test","subject":"s","body":"b"}`, nil); w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "organisation's storage is full") || strings.Contains(w.Body.String(), "outboxId") {
		t.Fatalf("send over the pool: %d %s", w.Code, w.Body)
	}
	if pending, err := boxA.PendingOutbound(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("a refused send was queued: %v %v", pending, err)
	}
	if got := importFailure(mailbox.ErrPoolFull); !strings.Contains(got, "organisation's storage is full") {
		t.Fatalf("import answer: %s", got)
	}
	// Pointing at KyIdentity enables SSO sign-in, which the send above did not need.
	idp := newStorageIdP(t)
	check(fx, srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL, ClientID: "kypost", ClientSecret: "suite-secret"}))
	fx.message(fx.a)
	fx.message(fx.other)
	if _, err := srv.storeContactPhoto(fx.other.ID, testPNG(t, 1)); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a photo before any grant: %v", err)
	}

	check(fx, srv.reportStorage(ctx))
	repA, okA := idp.report(orgA)
	repB, okB := idp.report(orgB)
	if _, staff := idp.report(staffOrg); staff || !okA || !okB {
		t.Fatalf("reported A %v, B %v, staff %v", okA, okB, staff)
	}
	usedA := orgUsageNow(t, srv, orgA)
	if repA.UsedBytes != usedA || usedA < 2*int64(len(poolDraft)) {
		t.Fatalf("org A reported %d, holds %d", repA.UsedBytes, usedA)
	}
	// Org B's figure holds none of org A's mail.
	if repB.UsedBytes != 0 {
		t.Fatalf("org B reported %d bytes it does not hold", repB.UsedBytes)
	}

	// A grant never lives past 15 minutes, whatever ttl_seconds says.
	idp.set(func(idp *storageIdP) { idp.ttl = 1 << 62 })
	check(fx, srv.reportStorage(ctx))
	if g, _ := tenancy.StorageGrant(ctx, db, orgA); !g.Expires.After(time.Now()) || g.Expires.After(time.Now().Add(tenancy.GrantTTL)) {
		t.Fatalf("grant expiry %v for a huge ttl", g.Expires)
	}
	idp.set(func(idp *storageIdP) { idp.ttl = 900 })

	// Within the grant every write path stores; the photo counts at once.
	check(fx, draft(boxA))
	if _, err := srv.storeContactPhoto(fx.other.ID, testPNG(t, 2)); err != nil {
		t.Fatal(err)
	}
	withPhoto := orgUsageNow(t, srv, orgA)
	if photos, _ := srv.contactPhotoBytesUsed(fx.other.ID); photos == 0 || withPhoto != usedA+int64(len(poolDraft))+photos {
		t.Fatalf("usage %d after a draft and %d photo bytes, from %d", withPhoto, photos, usedA)
	}

	// Room for exactly one more draft: it fits, the next does not, and nothing is stored.
	idp.set(func(idp *storageIdP) {
		idp.room = func(org string, _ int64) int64 {
			if org == orgA {
				return int64(len(poolDraft))
			}
			return 1 << 30
		}
	})
	check(fx, srv.reportStorage(ctx))
	check(fx, draft(boxA))
	full := orgUsageNow(t, srv, orgA)
	if err := draft(boxA); !errors.Is(err, mailbox.ErrPoolFull) || !errors.Is(err, mailbox.ErrCapacity) {
		t.Fatalf("one draft past the grant: %v", err)
	}
	if _, err := srv.storeContactPhoto(fx.a.ID, testPNG(t, 3)); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a photo past the grant: %v", err)
	}
	if now := orgUsageNow(t, srv, orgA); now != full {
		t.Fatalf("refused writes stored %d bytes", now-full)
	}
	// Org B has its own grant.
	check(fx, draft(boxB))
	// Mail receiving accepted imports over the pool.
	fx.message(fx.a)

	// A deletion frees room at once, with no new report.
	ids, err := boxA.List(ctx, "Drafts", 0, 10)
	check(fx, err)
	if len(ids) == 0 {
		t.Fatal("no draft to delete")
	}
	check(fx, boxA.Delete(ctx, "Drafts", ids[0].ID))
	// The held message imported above still takes the room the delete freed.
	check(fx, boxA.Delete(ctx, "INBOX", mustInboxID(t, boxA)))
	check(fx, draft(boxA))

	// KyIdentity down: the report fails and the grant stands until it expires.
	idp.set(func(idp *storageIdP) {
		idp.status, idp.room = http.StatusInternalServerError, func(string, int64) int64 { return 1 << 30 }
	})
	if err := srv.reportStorage(ctx); err == nil {
		t.Fatal("a failed report succeeded")
	}
	before, err := tenancy.StorageGrant(ctx, db, orgA)
	check(fx, err)
	// A stale answer keeps the grant too.
	idp.set(func(idp *storageIdP) { idp.status, idp.result = 0, "stale" })
	check(fx, srv.reportStorage(ctx))
	if after, _ := tenancy.StorageGrant(ctx, db, orgA); after != before {
		t.Fatalf("a stale answer changed the grant: %+v -> %+v", before, after)
	}
	// An expired grant fits nothing.
	_, err = db.ExecContext(ctx, `UPDATE org_storage SET ceiling_bytes = ?, expires_at = ? WHERE org_id = ?`, int64(1<<40), 1, orgA)
	check(fx, err)
	if err := draft(boxA); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a write on an expired grant: %v", err)
	}
	// KyIdentity no longer knows the org: its grant is zeroed.
	idp.set(func(idp *storageIdP) { idp.result = "unknown_org" })
	check(fx, srv.reportStorage(ctx))
	if g, _ := tenancy.StorageGrant(ctx, db, orgB); g.Ceiling != 0 {
		t.Fatalf("unknown org kept a grant: %+v", g)
	}
	if err := draft(boxB); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a write for an unknown org: %v", err)
	}
	// Over the pool KyIdentity grants zero (S5): nothing new is stored.
	idp.set(func(idp *storageIdP) { idp.result, idp.room = "", func(string, int64) int64 { return 0 } })
	check(fx, srv.reportStorage(ctx))
	if err := draft(boxB); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("a write with a zero grant: %v", err)
	}

	// The staff org is exempt, and an account with no org is refused.
	if err := srv.orgPoolRoom(ctx, staffOrg, 1<<40); err != nil {
		t.Fatalf("staff org: %v", err)
	}
	if err := srv.orgPoolRoom(ctx, "", 1); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("no org: %v", err)
	}
}

// orgUsageNow is org's usage as the write check computes it.
func orgUsageNow(t *testing.T, srv *Server, org string) int64 {
	t.Helper()
	ledger, err := srv.ssoLifecycle.NativeMailboxesWithoutAddresses()
	if err != nil {
		t.Fatal(err)
	}
	n, err := srv.orgUsage(context.Background(), org, ledger, func(string) (int64, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustInboxID(t *testing.T, box *mailbox.Store) int64 {
	t.Helper()
	ids, err := box.List(context.Background(), "INBOX", 0, 10)
	if err != nil || len(ids) == 0 {
		t.Fatalf("inbox: %v %v", ids, err)
	}
	return ids[0].ID
}

// Single-tenant: no report is sent, no write is checked, and nothing reaches tenancy.db.
func TestSingleTenantHasNoStoragePool(t *testing.T) {
	srv := newTestServer(t)
	idp := newStorageIdP(t)
	if err := srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL, ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.StartOrgPoller(ctx)
	if len(idp.reports) != 0 {
		t.Fatal("a single-tenant install reported storage")
	}
	if err := srv.orgPoolRoom(context.Background(), "", 1<<40); err != nil {
		t.Fatal(err)
	}
	if err := srv.StoragePoolCheck(context.Background(), mailbox.Owner{Issuer: "https://idp.example", Subject: "x"}, 1<<40); err != nil {
		t.Fatal(err)
	}
	u := srv.mustBootstrapUserID(t)
	if err := srv.photoPoolRoom(u, 1<<40); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.storeContactPhoto(u, testPNG(t, 4)); err != nil {
		t.Fatal(err)
	}
}

// Mail held for import counts in the report and in the write check, for the org whose
// mailbox it is bound to only.
func TestOrgStoragePoolCountsHeldMail(t *testing.T) {
	fx, _ := newOrgFixture(t)
	srv, db, ctx := fx.srv, fx.srv.tenancy, context.Background()
	holding, err := ingress.Open(filepath.Join(srv.stateDir, "receiving"), ingress.ReceivingLimits)
	check(fx, err)
	defer holding.Close()
	for _, r := range []struct{ address, subject, mailbox string }{
		{"member-a@example.test", "member-a", fx.a.ID}, {"member-b@example.test", "member-b", fx.b.ID},
	} {
		check(fx, holding.SetRoute(ctx, ingress.Route{Address: r.address, Issuer: srv.ssoStore.Load().IssuerURL, Subject: r.subject, Mailbox: r.mailbox, Generation: 1, Active: true, ValidUntil: time.Now().Add(time.Hour)}))
	}
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{"member-a@example.test"}, Subject: "held", Body: strings.Repeat("held ", 200)}.Build()
	check(fx, holding.Bind(ctx, purgeGateway, "held-a", "sender@outside.test", "member-a@example.test"))
	check(fx, holding.Accept(ctx, purgeGateway, "held-a", "sender@outside.test", bytes.NewReader(raw)))
	held, err := holding.HeldBytes(ctx, fx.a.ID)
	check(fx, err)
	if held == 0 {
		t.Fatal("nothing held")
	}

	idp := newStorageIdP(t)
	check(fx, srv.ssoStore.Save(sso.SSOSettings{Enabled: true, IssuerURL: idp.URL, ClientID: "kypost", ClientSecret: "suite-secret"}))
	check(fx, srv.reportStorage(ctx))
	stored := orgUsageNow(t, srv, orgA) // without the spool
	if a, _ := idp.report(orgA); a.UsedBytes != stored+held {
		t.Fatalf("org A reported %d, want %d stored + %d held", a.UsedBytes, stored, held)
	}
	if b, _ := idp.report(orgB); b.UsedBytes != 0 {
		t.Fatalf("org B reported org A's held mail: %d", b.UsedBytes)
	}
	// A ceiling with room for the write only if held mail were not counted.
	_, err = db.ExecContext(ctx, `UPDATE org_storage SET ceiling_bytes = ? WHERE org_id = ?`, stored+int64(len(poolDraft)), orgA)
	check(fx, err)
	if err := srv.orgPoolRoom(ctx, orgA, int64(len(poolDraft))); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("the write check ignored held mail: %v", err)
	}
	_, err = db.ExecContext(ctx, `UPDATE org_storage SET ceiling_bytes = ? WHERE org_id = ?`, stored+held+int64(len(poolDraft)), orgA)
	check(fx, err)
	if err := srv.orgPoolRoom(ctx, orgA, int64(len(poolDraft))); err != nil {
		t.Fatalf("a write that fits with held mail counted: %v", err)
	}
}
