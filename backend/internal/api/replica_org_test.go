package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// orgReplicas is two multi-tenant servers on one PostgreSQL, with two members of org A and
// one of org B provisioned on the first.
func orgReplicas(t *testing.T) (a, b *Server, a1, a2, b1 string) {
	t.Helper()
	dbbackend.RequirePostgres(t)
	a, _ = setupSSOTestServer(t)
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
	setOrg(t, dbA, orgB, "active", true, `{}`)
	a1 = provisionMember(t, a, dbA, "replica-a1", orgA).ID
	a2 = provisionMember(t, a, dbA, "replica-a2", orgA).ID
	b1 = provisionMember(t, a, dbA, "replica-b1", orgB).ID
	b = replicaOf(t, a)
	dbB, err := tenancy.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbB.Close() })
	b.tenancy, b.staffOrg, b.users.StaffOrg = dbB, staffOrg, staffOrg
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	return a, b, a1, a2, b1
}

// TestOrgQuotas across two replicas (MH6): an org's send caps admit the same total however
// its members' sends are spread over the servers, and refuse the next one on both.
func TestTwoReplicasShareOrgQuotas(t *testing.T) {
	a, b, a1, a2, b1 := orgReplicas(t)
	ctx, now := context.Background(), time.Now()
	setOrg(t, a.tenancy, orgA, "active", true, `{"sendPerHour":3}`)
	for i, step := range []struct {
		srv  *Server
		user string
	}{{a, a1}, {b, a2}, {b, a1}} {
		if _, err := step.srv.chargeSend(ctx, "s"+string(rune('0'+i)), step.user, 1, now); err != nil {
			t.Fatalf("send %d of 3: %v", i+1, err)
		}
	}
	for _, srv := range []*Server{a, b} {
		if _, err := srv.chargeSend(ctx, "over-"+srv.instanceID, a2, 1, now); !errors.Is(err, state.ErrSendQuota) {
			t.Fatalf("a fourth org send in the hour: %v", err)
		}
	}
	if _, err := b.chargeSend(ctx, "other-org", b1, 1, now); err != nil {
		t.Fatalf("org B is not capped by A: %v", err)
	}
}

// One import or export per org at a time across replicas; another org is not held up, and a
// released or expired lease frees the org.
func TestTwoReplicasHoldOneOrgTransfer(t *testing.T) {
	a, b, a1, a2, b1 := orgReplicas(t)
	release, status, _ := a.takeOrgTransfer(a1)
	if status != 0 {
		t.Fatalf("first transfer: %d", status)
	}
	if _, status, refusal := b.takeOrgTransfer(a2); status != http.StatusConflict || refusal != orgTransferBusy {
		t.Fatalf("second transfer in the org on another replica: %d %q", status, refusal)
	}
	// The import claim takes the same lease: a member's import on B waits for A's export.
	if status, refusal, _ := b.claimImport(&importJob{State: "uploading", user: a2}); status != http.StatusConflict || refusal != orgTransferBusy {
		t.Fatalf("an import beside the org's transfer: %d %q", status, refusal)
	}
	otherRelease, status, _ := b.takeOrgTransfer(b1)
	if status != 0 {
		t.Fatalf("another org's transfer: %d", status)
	}
	otherRelease()
	release()
	release() // idempotent
	again, status, _ := b.takeOrgTransfer(a2)
	if status != 0 {
		t.Fatalf("after release: %d", status)
	}
	again()
	// A replica that died never releases; its lease runs out.
	prev := orgTransferLease
	orgTransferLease = 50 * time.Millisecond
	t.Cleanup(func() { orgTransferLease = prev })
	dead, status, _ := a.takeOrgTransfer(a1)
	if status != 0 {
		t.Fatalf("short lease: %d", status)
	}
	t.Cleanup(dead) // stops its renewal; the lease is no longer its to give back
	time.Sleep(100 * time.Millisecond)
	if again, status, _ := b.takeOrgTransfer(a2); status != 0 {
		t.Fatalf("after the dead holder's lease expired: %d", status)
	} else {
		again()
	}
}
