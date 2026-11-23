package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
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

	if err = control.QueryRow(orgFencePIDQuery, state.FenceOrg+orgA).Scan(&pid); err != nil {
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
	if err = control.QueryRow(orgFencePIDQuery, state.FenceOrg+orgA).Scan(&holder); err != nil {
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
