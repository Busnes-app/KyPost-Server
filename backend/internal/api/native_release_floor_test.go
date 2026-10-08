package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// retainDirectory records a signed resource without running the webhook's
// account mutation: the state a restored directory row is left in.
func retainDirectory(t *testing.T, s *Server, user map[string]any, revision int) {
	t.Helper()
	user["meta"] = map[string]any{"version": fmt.Sprintf(`W/"%d"`, revision)}
	raw, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: fmt.Sprintf("retained-%d", revision), Type: "user.updated", At: time.Now()}
	if _, err := s.ssoLifecycle.ApplyDirectoryUser(separationIssuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
}

func floorSubject(t *testing.T, s *Server, revision int64, active bool) {
	t.Helper()
	if err := s.ssoLifecycle.RecordNativeReleaseFloors(separationIssuer, map[string]sso.NativeReleaseFloor{"native-runtime-one": {Revision: revision, Active: active}}); err != nil {
		t.Fatal(err)
	}
}

func assertFloorRefusals(t *testing.T, refused []error) {
	t.Helper()
	for _, err := range refused {
		if !errors.Is(err, sso.ErrNativeReleaseFloor) {
			t.Fatal("worker pass not refused by floor:", err)
		}
	}
}

// Case A: a deleted administrator's stale directory row cannot recreate the
// account; a strictly newer revision can.
func TestReleaseFloorRefusesStaleAdministratorRow(t *testing.T) {
	s := newNativeRuntimeServer(t)
	retainDirectory(t, s, adminRuntimeUser(true), 1)
	for _, floor := range []int64{1, 2} {
		floorSubject(t, s, floor, false)
		refused := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one")
		if _, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
			t.Fatalf("floor %d provisioned administrator: %v", floor, err)
		}
		if !errors.Is(refused, sso.ErrNativeReleaseFloor) {
			t.Fatalf("floor %d: %v", floor, refused)
		}
	}
	retainDirectory(t, s, adminRuntimeUser(true), 3)
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
}

// Case B: an unpublished subject the evidence called inactive is not allocated
// from its stale active row.
func TestReleaseFloorRefusesUnpublishedAllocation(t *testing.T) {
	s := newNativeRuntimeServer(t)
	retainDirectory(t, s, runtimeDirectoryUser(true), 1)
	floorSubject(t, s, 1, false)
	var refused []error
	for range 2 {
		refused = append(refused, s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"))
	}
	if _, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("allocated below floor: %v", err)
	}
	if _, reserved, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one"); err != nil || reserved {
		t.Fatal("reserved below floor", reserved, err)
	}
	assertFloorRefusals(t, refused)
}

// Case C: a published account the repair deactivated is not reallocated from
// a pending active row, and its assignment stays where it was.
func TestReleaseFloorKeepsRepairedAccountInactive(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "floor-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil || u.NativeMailboxSource == "" {
		t.Fatal("native publication", err)
	}
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, errors.New("DNS unavailable") })
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.updated", "floor-pending", 2, runtimeDirectoryUser(true)))
	s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := s.nativeDomains.Read()
		return []string{d.RecordValue()}, err
	})
	if _, err := s.users.Deactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one")
	if err != nil || before.Revision != 1 {
		t.Fatal("assignment precondition", before.Revision, err)
	}
	floorSubject(t, s, 2, false)
	var refused []error
	for range 2 {
		refused = append(refused, s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"))
	}
	after, _, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one")
	if err != nil || after.Revision != before.Revision || after.Status != before.Status || after.DesiredActive != before.DesiredActive {
		t.Fatalf("floored assignment changed: %+v -> %+v %v", before, after, err)
	}
	if current, err := s.users.Get(u.ID); err != nil || current.Active {
		t.Fatal("floored account reactivated", err)
	}
	assertFloorRefusals(t, refused)
}

// Deactivation at or below the floor still converges: the disable path is
// the one thing a floor never blocks.
func TestReleaseFloorAllowsDisable(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "floor-create", 1, runtimeDirectoryUser(true)))
	retainDirectory(t, s, runtimeDirectoryUser(false), 2)
	floorSubject(t, s, 2, false)
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	if a, _, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one"); err != nil || a.Revision != 2 || a.DesiredActive {
		t.Fatalf("disable did not converge: %+v %v", a, err)
	}
}
