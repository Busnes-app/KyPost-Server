package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// TestMemberErasurePurgesOnlyThatMember: a household member's signed user.erased removes
// their account, mailboxes, addresses and data, and nothing of the other member or org B;
// a repeat is a no-op, and the subject never returns (K "Delete everything").
func TestMemberErasurePurgesOnlyThatMember(t *testing.T) {
	px := newPurgeFixture(t)
	ctx := context.Background()
	srv := px.srv
	aDigest := px.orgDigest(px.a)
	if w := postDirectory(t, srv, testSyncKey, "user.erased", "ev-erase", 2, orgUser("member-a2", orgA, false)); w.Code != http.StatusOK {
		t.Fatal("user.erased", w.Code, w.Body)
	}
	if _, err := srv.users.Get(px.other.ID); err != nil {
		t.Fatal("the erased member's account went before the purge ran", err)
	}
	if err := srv.runPurges(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.users.Get(px.other.ID); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("the erased member's account survived", err)
	}
	for _, dir := range []string{config.UserDir(srv.configDir, px.other.ID), filepath.Join(srv.stateDir, "mailboxes", px.other.extra)} {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived: %v", dir, err)
		}
	}
	for _, a := range []string{"member-a2@example.test", "member-a2-box@example.test"} {
		if _, err := srv.ssoLifecycle.AddNativeAlias(ctx, srv.stateDir, px.b.extra, a); !errors.Is(err, sso.ErrNativeAddressConflict) {
			t.Fatalf("erased %s reissued: %v", a, err)
		}
	}
	// The other member and org B are untouched.
	if after := px.orgDigest(px.a); after != aDigest {
		t.Fatalf("the other household member changed:\n%s", orgDigestDiff(aDigest, after))
	}
	if after := px.orgDigest(px.b); after != px.bDigest {
		t.Fatalf("org B changed:\n%s", orgDigestDiff(px.bDigest, after))
	}
	if addresses, err := srv.ssoLifecycle.NativeAddresses(); err != nil || addresses["member-a@example.test"].Mailbox != px.a.ID {
		t.Fatal("the other member's address was touched", err)
	}
	// A repeat, and an active event for the same subject, change nothing.
	if w := postDirectory(t, srv, testSyncKey, "user.erased", "ev-erase", 2, orgUser("member-a2", orgA, false)); w.Code != http.StatusOK {
		t.Fatal("repeated user.erased", w.Code, w.Body)
	}
	postDirectory(t, srv, testSyncKey, "user.updated", "ev-back", 9, orgUser("member-a2", orgA, true))
	if _, err := srv.users.GetBySSOSub("member-a2"); !errors.Is(err, users.ErrNotFound) {
		t.Fatal("an erased subject was provisioned again", err)
	}
	if live, err := tenancy.LivePurges(ctx, px.db); err != nil || len(live) != 0 {
		t.Fatal("a repeat started another purge", live, err)
	}
}

// TestMemberErasureRefusals: an erasure naming another org than the subject's is refused and
// records nothing; an active user.erased is malformed; a single-tenant install refuses the
// type as before.
func TestMemberErasureRefusals(t *testing.T) {
	px := newPurgeFixture(t)
	ctx := context.Background()
	if w := postDirectory(t, px.srv, testSyncKey, "user.erased", "ev-cross", 2, orgUser("member-b", orgA, false)); w.Code != http.StatusUnprocessableEntity {
		t.Fatal("an erasure for another org's subject", w.Code, w.Body)
	}
	if w := postDirectory(t, px.srv, testSyncKey, "user.erased", "ev-active", 2, orgUser("member-a2", orgA, true)); w.Code != http.StatusBadRequest {
		t.Fatal("an active user.erased", w.Code, w.Body)
	}
	if w := postDirectory(t, px.srv, testSyncKey, "user.erased", "ev-noorg", 2, orgUser("member-a2", "", false)); w.Code != http.StatusBadRequest {
		t.Fatal("a user.erased without org_id", w.Code, w.Body)
	}
	if live, err := tenancy.LivePurges(ctx, px.db); err != nil || len(live) != 0 {
		t.Fatal("a refused erasure recorded a purge", live, err)
	}
	if _, err := px.srv.users.Get(px.b.ID); err != nil {
		t.Fatal(err)
	}

	single := newDirectoryTestServer(t)
	directoryStatus(t, postDirectory(t, single, testSyncKey, "user.created", "ev-1", 1, scimUser("solo", "solo", true)))
	if w := postDirectory(t, single, testSyncKey, "user.erased", "ev-2", 2, scimUser("solo", "solo", false)); w.Code != http.StatusBadRequest {
		t.Fatal("single-tenant accepted user.erased", w.Code, w.Body)
	}
	if u, err := single.users.GetBySSOSub("solo"); err != nil || !u.Active {
		t.Fatal("single-tenant user.erased changed the account", err)
	}
}
