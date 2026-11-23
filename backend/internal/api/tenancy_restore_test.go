package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// heldAfterRestore puts px where a restore leaves a multi install: the restored org cache
// and finished purges forgotten (tenancy.FenceRestored) and native mail held.
func heldAfterRestore(t *testing.T, px *purgeFixture) string {
	t.Helper()
	if err := tenancy.ForgetRestored(context.Background(), px.db); err != nil {
		t.Fatal(err)
	}
	epoch, err := fsutil.NewUUIDv4()
	if err != nil {
		t.Fatal(err)
	}
	hold, _ := json.Marshal(map[string]any{"version": 1, "epoch": epoch, "reason": "restore"})
	if err := os.WriteFile(filepath.Join(px.srv.stateDir, sso.NativeRestoreHoldFile), hold, 0o600); err != nil {
		t.Fatal(err)
	}
	return epoch
}

// TestRestoreReleaseWaitsForKyIdentity: after a restore, nothing of a customer org is served
// and staff cannot release the hold until a poll of every org from revision 0 and the
// erased-subject list both succeed. KyIdentity down keeps it so; once it answers, the
// deleted org and the erased member are purged again and the release passes this gate
// (K "Delete everything" tests).
func TestRestoreReleaseWaitsForKyIdentity(t *testing.T) {
	px := newPurgeFixture(t)
	ctx := context.Background()
	srv := px.srv
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-staff", 1, orgUser("staff-1", staffOrg, true, "kypost.admin")))
	staff, err := srv.users.GetBySSOSub("staff-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.ClearMustChangePassword(staff.ID); err != nil {
		t.Fatal(err)
	}
	// An unaffected tenant member, whose mailboxes must stay qualified.
	kept := orgUser("member-b3", orgB, true)
	directoryStatus(t, postDirectory(t, srv, testSyncKey, "user.created", "ev-b3", 1, kept))
	b3, err := srv.users.GetBySSOSub("member-b3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ssoLifecycle.CreateNativeMailbox(ctx, srv.stateDir, b3.ID, "member-b3-box@example.test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KYPOST_NATIVE_RESTORE_RELEASE", "true")
	epoch := heldAfterRestore(t, px)
	// The restore's qualification marker is written on restore staging, which stays SQLite
	// files on PostgreSQL (backup.QuarantineNativeRestore): only SQLite records one here.
	staged := !dbbackend.OnPostgresTest()
	var restored map[string]sso.NativeRestoreMailbox
	if staged {
		// The restore's last stage: every ledger mailbox gets a fresh reference generation, and
		// the qualification marker records them.
		restored, err = srv.ssoLifecycle.NativeRestoreMailboxes(srv.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range restored {
			if err := mailbox.RotateRestoredMessageReferences(filepath.Join(m.Dir, "mailbox", "mailbox.db"), m.Source); err != nil {
				t.Fatal(err)
			}
		}
		if err := srv.ssoLifecycle.RecordNativeRestoreQualification(srv.stateDir, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	idp := newOrgsIdP(t)
	pointAtOrgs(t, srv, idp, "suite-secret")
	idp.set(http.StatusServiceUnavailable)
	const gate = "KyIdentity has not been read in full since this restore"
	release := func() *httptest.ResponseRecorder {
		return px.staffCall(staff, http.MethodPost, "/api/admin/native-recovery/release", `{}`, "")
	}

	// KyIdentity unreachable: no read recorded, the hold cannot be released, sign-in is
	// refused and receiving answers a temporary refusal.
	if err := srv.restoreReads(ctx, epoch, time.Now()); err == nil {
		t.Fatal("restore reads passed with KyIdentity down")
	}
	if read, err := tenancy.RestoreRead(ctx, px.db, epoch); err != nil || read {
		t.Fatal("a failed read was recorded", err)
	}
	if w := release(); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), gate) {
		t.Fatalf("release with KyIdentity down: %d %s", w.Code, w.Body)
	}
	if w := px.call(px.b, http.MethodGet, "/api/contacts", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatal("a member was served before KyIdentity confirmed the org", w.Code)
	}
	if o := orgRow(t, px.db, orgB); o.Receiving() != tenancy.ReceiveTemporary {
		t.Fatal("receiving did not answer a temporary refusal for an unconfirmed org")
	}

	// KyIdentity answers: org A was deleted and B's member erased after the backup.
	idp.mu.Lock()
	idp.erased = []erasedSubject{{Sub: "member-b", OrgID: orgB, Revision: 1}}
	idp.mu.Unlock()
	idp.set(0, listed(orgA, suitedir.OrgDeleted, 5), listed(orgB, suitedir.OrgActive, 6))
	if err := srv.restoreReads(ctx, epoch, time.Now()); err != nil {
		t.Fatal(err)
	}
	if idp.lastSince() != "0" {
		t.Fatal("the restore poll did not start from revision 0:", idp.lastSince())
	}
	if read, err := tenancy.RestoreRead(ctx, px.db, epoch); err != nil || !read {
		t.Fatal("the restore read was not recorded", err)
	}
	// Once recorded, the hold's polls are the ordinary incremental ones again.
	if err := srv.restoreReads(ctx, epoch, time.Now()); err != nil {
		t.Fatal(err)
	}
	if since := idp.lastSince(); since == "0" {
		t.Fatal("a recorded restore read still polled every org from revision 0")
	}
	if err := srv.runPurges(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []orgMember{px.a, px.other, px.b} {
		if _, err := srv.users.Get(m.ID); !errors.Is(err, users.ErrNotFound) {
			t.Fatalf("%s survived the purges after the restore: %v", m.Username, err)
		}
	}
	if live, err := tenancy.LivePurges(ctx, px.db); err != nil || len(live) != 0 {
		t.Fatal("purges after the restore did not finish", live, err)
	}
	if _, err := srv.users.Get(b3.ID); err != nil {
		t.Fatal("the unaffected member was purged:", err)
	}
	if staged {
		// The purged mailboxes leave the restore qualification; every other one keeps it.
		q, err := srv.ssoLifecycle.CheckNativeRestoreQualification(srv.stateDir)
		if err != nil {
			t.Fatal("the purges after the restore broke its qualification:", err)
		}
		var survivors []sso.NativeRestoreMailbox
		for id, m := range restored {
			gone, err := tenancy.MailboxPurged(ctx, px.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, listed := q.Mailboxes[id]; listed == gone {
				t.Fatalf("mailbox %s: purged %v, still qualified %v", id, gone, listed)
			}
			if !gone {
				survivors = append(survivors, m)
			}
		}
		if len(survivors) != 2 || len(restored) != 8 {
			t.Fatalf("%d of %d mailboxes survived, want b3's 2 of 8", len(survivors), len(restored))
		}
		// A survivor keeps the generation check.
		if err := mailbox.RotateRestoredMessageReferences(filepath.Join(survivors[0].Dir, "mailbox", "mailbox.db"), survivors[0].Source); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.ssoLifecycle.CheckNativeRestoreQualification(srv.stateDir); err == nil || !strings.Contains(err.Error(), "reference generation changed") {
			t.Fatal("a surviving mailbox passed with a changed generation:", err)
		}
	}
	if w := release(); strings.Contains(w.Body.String(), gate) {
		t.Fatalf("release still refused by the restore read: %d %s", w.Code, w.Body)
	}
	// An unreadable hold is never released this way.
	holdPath := filepath.Join(srv.stateDir, sso.NativeRestoreHoldFile)
	good, err := os.ReadFile(holdPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holdPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if why, err := srv.restoreReleaseRefusal(ctx); err != nil || !strings.Contains(why, gate) {
		t.Fatal("an unreadable hold passed the restore gate", why, err)
	}
	if err := os.WriteFile(holdPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	// A read recorded for this hold does not count for the next restore.
	if err := tenancy.ForgetRestored(ctx, px.db); err != nil {
		t.Fatal(err)
	}
	if w := release(); !strings.Contains(w.Body.String(), gate) {
		t.Fatalf("a forgotten read still released: %d %s", w.Code, w.Body)
	}
}

// TestSuiteErasedRefusesMalformedAnswers: one bad entry, a cursor that does not match, a
// refused credential, an oversize body, a redirect or a non-HTTPS issuer fails the read.
func TestSuiteErasedRefusesMalformedAnswers(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"bad org":         `{"subjects":[{"sub":"s1","org_id":"ORG","revision":1}]}`,
		"empty sub":       `{"subjects":[{"sub":"","org_id":"` + orgA + `","revision":1}]}`,
		"no progress":     `{"subjects":[{"sub":"s1","org_id":"` + orgA + `","revision":2},{"sub":"s2","org_id":"` + orgA + `","revision":2}]}`,
		"no list":         `{}`,
		"cursor mismatch": `{"subjects":[{"sub":"s1","org_id":"` + orgA + `","revision":1}],"next":"9"}`,
		"oversize":        `{"subjects":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		if _, err := suiteErased(ctx, suitedir.Client{Issuer: srv.URL, ClientID: "kypost", ClientSecret: "s"}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refused.Close()
	if _, err := suiteErased(ctx, suitedir.Client{Issuer: refused.URL}); !errors.Is(err, suitedir.ErrUnauthorized) {
		t.Error("401:", err)
	}
	redirect := httptest.NewServer(http.RedirectHandler("http://127.0.0.1:1/", http.StatusFound))
	defer redirect.Close()
	if _, err := suiteErased(ctx, suitedir.Client{Issuer: redirect.URL}); err == nil {
		t.Error("followed a redirect")
	}
	if _, err := suiteErased(ctx, suitedir.Client{Issuer: "http://identity.example"}); !errors.Is(err, suitedir.ErrInsecureIssuer) {
		t.Error("plain HTTP issuer:", err)
	}
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "0" {
			_, _ = w.Write([]byte(`{"subjects":[{"sub":"s1","org_id":"` + orgA + `","revision":3}],"next":"3"}`))
			return
		}
		_, _ = w.Write([]byte(`{"subjects":[{"sub":"s2","org_id":"` + orgB + `","revision":4}]}`))
	}))
	defer two.Close()
	if got, err := suiteErased(ctx, suitedir.Client{Issuer: two.URL}); err != nil || len(got) != 2 || got[1].Sub != "s2" {
		t.Fatal("two pages:", got, err)
	}
}
