package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTablePurgeRechecksLateAccountAfterOrgFenceLoss(t *testing.T) {
	dbbackend.RequirePostgres(t)
	px := newPurgeFixture(t)
	s := px.srv
	mountServerDocuments(t, s)
	accounts, err := volumes.OpenAccounts(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	entered, lost, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	late := users.User{ID: "late-fenced-account", Username: "late-fenced-account", Role: users.RoleUser, Active: true, SSOIssuer: px.a.SSOIssuer, SSOSub: "late-fenced-subject", OrgID: orgA}
	go func() {
		done <- s.withAccountOrgFence(context.Background(), orgA, true, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(lost)
			<-resume
			// Deliberately ignore the lost fence, as a stalled writer with retained
			// inputs might. The final purge recheck is the backstop for this case.
			raw, _ := json.Marshal(late)
			if _, err := accounts.Exec(`INSERT INTO accounts(id,username,issuer,subject,record) VALUES(?,?,?,?,?)`, late.ID, late.Username, late.SSOIssuer, late.SSOSub, raw); err != nil {
				return err
			}
			if err := tenancy.BindSubject(context.Background(), s.tenancy, late.SSOIssuer, late.SSOSub, orgA); err != nil {
				return err
			}
			if err := tenancy.BindUser(context.Background(), s.tenancy, late.ID, orgA); err != nil {
				return err
			}
			return vol.OpenDocument(filepath.Join(s.userStateDir(late.ID), "contacts.json")).Replace([]byte(`{"contacts":[]}`))
		})
	}()
	<-entered
	schema, _, err := dbbackend.SchemaName(filepath.Join(s.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := dbbackend.Pool(schema, false)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var pid int

	if err = control.QueryRow(orgFencePIDQuery, vol.FenceOrg+orgA).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var killed bool
	if err = control.QueryRow(`SELECT pg_terminate_backend(?)`, pid).Scan(&killed); err != nil || !killed {
		t.Fatal(killed, err)
	}
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("fence loss did not cancel the holder")
	}
	setOrg(t, s.tenancy, orgA, "deleted", false, `{}`)
	if err = s.beginPurge(context.Background(), tenancy.PurgeScope{Org: orgA}, time.Now()); err != nil {
		t.Fatal(err)
	}
	injected := false
	purgeStepHook = func(step int) error {
		if step == -5 && !injected {
			injected = true
			close(resume)
			return <-done
		}
		return nil
	}
	defer func() { purgeStepHook = nil }()
	if err = s.runPurges(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !injected {
		t.Fatal("late writer was not exercised")
	}
	if _, err = s.users.Get(late.ID); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("late account survived", err)
	}
	if got := userOrgRow(t, s.tenancy, late.ID); got != "" {
		t.Fatal("late user binding survived", got)
	}
	var remaining int
	if err = accounts.QueryRow(`SELECT count(*) FROM documents WHERE owner=?`, late.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("late document survived", remaining, err)
	}
	if got, err := s.users.Get(px.b.ID); err != nil || got.OrgID != orgB {
		t.Fatal("other org changed", got, err)
	}
	if err := s.checkAccountOrg(context.Background(), late, orgA); !errors.Is(err, tenancy.ErrNotServed) {
		t.Fatal("stale identity recreated post-purge authority", err)
	}
	if got := userOrgRow(t, s.tenancy, late.ID); got != "" {
		t.Fatal("stale identity rebound a purged account")
	}
	live, err := tenancy.LivePurges(context.Background(), s.tenancy)
	if err != nil || len(live) != 0 {
		t.Fatal(live, err)
	}
}

func TestTablePurgeSnapshotWaitsForThreeRecordProvisioning(t *testing.T) {
	dbbackend.RequirePostgres(t)
	px := newPurgeFixture(t)
	s := px.srv
	mountServerDocuments(t, s)
	entered, resume := make(chan struct{}), make(chan struct{})
	provisioned := make(chan error, 1)
	var late users.User
	go func() {
		provisioned <- s.withAccountOrgFence(context.Background(), orgA, true, func(ctx context.Context) error {
			issuer := px.a.SSOIssuer
			if err := tenancy.BindSubject(ctx, s.tenancy, issuer, "ordered-subject", orgA); err != nil {
				return err
			}
			var err error
			late, err = s.users.CreateSSOUserIssuer(issuer, "ordered-account", users.RoleUser, "ordered-subject", "", "", orgA)
			if err != nil {
				return err
			}
			close(entered)
			<-resume
			return s.bindNewAccount(ctx, late)
		})
	}()
	select {
	case <-entered:
	case err := <-provisioned:
		t.Fatal("provisioning refused before its pause", err)
	case <-time.After(5 * time.Second):
		t.Fatal("provisioning did not reach its pause")
	}
	schema, _, err := dbbackend.SchemaName(filepath.Join(s.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := dbbackend.Pool(schema, false)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var holder int
	if err = control.QueryRow(orgFencePIDQuery, vol.FenceOrg+orgA).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	snapshotted := make(chan error, 1)
	go func() { snapshotted <- s.beginPurge(context.Background(), tenancy.PurgeScope{Org: orgA}, time.Now()) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case err := <-snapshotted:
			t.Fatalf("snapshot crossed unfinished provisioning: %v", err)
		default:
		}
		var waiting bool
		if err = control.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND ?=ANY(pg_blocking_pids(pid)))`, holder).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot never waited on provisioning")
		}
		time.Sleep(time.Millisecond)
	}
	close(resume)
	if err = <-provisioned; err != nil {
		t.Fatal(err)
	}
	if err = <-snapshotted; err != nil {
		t.Fatal(err)
	}
	snap, err := tenancy.PurgeMembers(context.Background(), s.tenancy, tenancy.PurgeScope{Org: orgA})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range snap.Users {
		found = found || id == late.ID
	}
	if !found {
		t.Fatal("snapshot omitted completed provisioning", snap)
	}
}

const orgFencePIDQuery = `SELECT pid FROM pg_locks CROSS JOIN (SELECT ('x'||left(encode(sha256(convert_to(current_schema()||':'||?, 'UTF8')),'hex'),16))::bit(64)::bigint AS k) AS named WHERE locktype='advisory' AND objsubid=1 AND granted AND classid::bigint=((k>>32)&4294967295) AND objid::bigint=(k&4294967295)`

func TestTableStaffProvisioningKeepsMailboxlessCacheExemption(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fx, _ := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	staff := provisionMember(t, fx.srv, fx.srv.tenancy, "table-staff", staffOrg, "kypost.admin")
	if staff.Role != users.RoleAdmin || staff.NativeMailboxIssuer != "" || staff.NativeMailboxSource != "" {
		t.Fatal("staff account acquired ordinary mailbox authority", staff)
	}
}

func TestSyncRejectsForeignOrgBeforeAccountMutation(t *testing.T) {
	for _, mode := range []string{"files", "tables"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "tables" {
				dbbackend.RequirePostgres(t)
			}
			for _, eventType := range []string{"user.updated", "user.mfa_reset"} {
				t.Run(eventType, func(t *testing.T) {
					fx, _ := newOrgFixture(t)
					id, secret := pairNativeDevice(t, fx.srv, fx.a.ID, "foreign-org-sync-device")
					if mode == "tables" {
						mountServerDocuments(t, fx.srv)
					}
					before, err := fx.srv.users.Get(fx.a.ID)
					if err != nil {
						t.Fatal(err)
					}
					directory, known, err := fx.srv.ssoLifecycle.Directory(before.SSOIssuer, before.SSOSub)
					if err != nil || !known {
						t.Fatal(known, err)
					}
					response := postDirectory(t, fx.srv, testSyncKey, eventType, "foreign-org-at-boundary", 2, orgUser(before.SSOSub, orgB, false))
					if response.Code != http.StatusUnprocessableEntity {
						t.Fatal(response.Code, response.Body)
					}
					after, err := fx.srv.users.Get(before.ID)
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatal("foreign event changed account", err)
					}
					current, known, err := fx.srv.ssoLifecycle.Directory(before.SSOIssuer, before.SSOSub)
					if err != nil || !known || !reflect.DeepEqual(directory, current) {
						t.Fatal("foreign event advanced directory state", err)
					}
					if got := userOrgRow(t, fx.srv.tenancy, before.ID); got != orgA {
						t.Fatal("user binding changed", got)
					}
					if got := subjectOrgRow(t, fx.srv, fx.srv.tenancy, before.SSOSub); got != orgA {
						t.Fatal("subject binding changed", got)
					}
					if _, _, ok, _ := fx.srv.deviceAuthFromRequest(deviceRequest(id, secret)); !ok {
						t.Fatal("foreign event revoked the member's device")
					}
				})
			}
		})
	}
}

func TestTableStaffCacheExemptionAtEveryProvisioningBoundary(t *testing.T) {
	dbbackend.RequirePostgres(t)
	for _, boundary := range []string{"webhook", "provision", "reconcile"} {
		t.Run(boundary, func(t *testing.T) {
			fx, _ := newOrgFixture(t)
			s := fx.srv
			issuer := s.ssoStore.Load().IssuerURL
			const subject = "staff-without-cache"
			if boundary == "reconcile" {
				provisionMember(t, s, s.tenancy, subject, staffOrg, "kypost.admin")
			}
			mountServerDocuments(t, s)
			if boundary == "reconcile" {
				u, err := s.users.GetBySSOSubIssuer(issuer, subject)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.users.RemoveUsers([]string{u.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.tenancy.Exec(`ALTER TABLE orgs RENAME TO unavailable_orgs`); err != nil {
				t.Fatal(err)
			}
			// Staff still uses the shared org fence; only customer-cache reads are exempt.
			switch boundary {
			case "webhook":
				directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "cacheless-staff", 1, orgUser(subject, staffOrg, true, "kypost.admin")))
			case "provision":
				if err := tenancy.BindSubject(context.Background(), s.tenancy, issuer, subject, staffOrg); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(orgUser(subject, staffOrg, true, "kypost.admin"))
				var desired sso.DirectoryUser
				if err := json.Unmarshal(raw, &desired); err != nil {
					t.Fatal(err)
				}
				if err := s.withAccountOrgFence(context.Background(), staffOrg, true, func(ctx context.Context) error {
					return s.provisionDirectoryUser(ctx, issuer, desired, users.RoleAdmin)
				}); err != nil {
					t.Fatal(err)
				}
			case "reconcile":
				if err := s.reconcileNativeSubject(context.Background(), issuer, subject); err != nil {
					t.Fatal(err)
				}
			}
			staff, err := s.users.GetBySSOSubIssuer(issuer, subject)
			if err != nil || staff.Role != users.RoleAdmin || staff.OrgID != staffOrg || staff.NativeMailboxIssuer != "" || staff.NativeMailboxSource != "" {
				t.Fatal("cacheless staff provisioning", staff, err)
			}
			if userOrgRow(t, s.tenancy, staff.ID) != staffOrg || subjectOrgRow(t, s, s.tenancy, subject) != staffOrg {
				t.Fatal("staff bindings differ")
			}
			if _, exists, err := s.ssoLifecycle.NativeAssignment(issuer, subject); err != nil || exists {
				t.Fatal("staff acquired a mailbox", exists, err)
			}
		})
	}
}

func TestTableStaffWebhookKeepsOrgFenceWhenCacheIsUnavailable(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fx, _ := newOrgFixture(t)
	s := fx.srv
	mountServerDocuments(t, s)
	if _, err := s.tenancy.Exec(`ALTER TABLE orgs RENAME TO unavailable_orgs`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- s.globalStore.WithFences(ctx, []state.Fence{{Name: vol.FenceOrg + staffOrg}}, func(ctx context.Context) error {
			close(entered)
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	schema, _, err := dbbackend.SchemaName(filepath.Join(s.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := dbbackend.Pool(schema, false)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var holder int
	if err = control.QueryRow(orgFencePIDQuery, vol.FenceOrg+staffOrg).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- postDirectory(t, s, testSyncKey, "user.created", "fenced-cacheless-staff", 1, orgUser("fenced-staff", staffOrg, true, "kypost.admin"))
	}()
	for {
		select {
		case got := <-response:
			t.Fatalf("staff webhook bypassed org fence: %d %s", got.Code, got.Body)
		default:
		}
		var waiting bool
		if err = control.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND ?=ANY(pg_blocking_pids(pid)))`, holder).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	close(resume)
	if err = <-holderDone; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-response:
		directoryStatus(t, got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
