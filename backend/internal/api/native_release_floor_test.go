package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
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

// newFloorServer captures logs: a floor refusal is a quiet no-op, logged once.
func newFloorServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	s := newNativeRuntimeServer(t)
	logs := &bytes.Buffer{}
	logger, err := logging.NewWithOutput(logs)
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger
	return s, logs
}

// workerPasses runs the worker twice; each pass must be a quiet no-op.
func workerPasses(t *testing.T, s *Server) {
	t.Helper()
	for range 2 {
		if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
			t.Fatal("floor refusal is not a quiet no-op:", err)
		}
	}
}

func assertFloorLoggedOnce(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	if n := strings.Count(logs.String(), "release floor"); n != 1 || !strings.Contains(logs.String(), "native-runtime-one") {
		t.Fatalf("floor refusal logged %d times: %s", n, logs.String())
	}
}

// Case A: a deleted administrator's stale directory row cannot recreate the
// account; a strictly newer revision can.
func TestReleaseFloorRefusesStaleAdministratorRow(t *testing.T) {
	s, logs := newFloorServer(t)
	retainDirectory(t, s, adminRuntimeUser(true), 1)
	for _, floor := range []int64{1, 2} {
		floorSubject(t, s, floor, false)
		workerPasses(t, s)
		if _, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
			t.Fatalf("floor %d provisioned administrator: %v", floor, err)
		}
	}
	assertFloorLoggedOnce(t, logs)
	retainDirectory(t, s, adminRuntimeUser(true), 3)
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
		t.Fatal(err)
	}
	assertMailboxless(t, s, "native-runtime-one", users.RoleAdmin)
}

// Case B: an unpublished subject the evidence called inactive is not allocated
// from its stale active row.
func TestReleaseFloorRefusesUnpublishedAllocation(t *testing.T) {
	s, logs := newFloorServer(t)
	retainDirectory(t, s, runtimeDirectoryUser(true), 1)
	floorSubject(t, s, 1, false)
	workerPasses(t, s)
	if _, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("allocated below floor: %v", err)
	}
	if _, reserved, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one"); err != nil || reserved {
		t.Fatal("reserved below floor", reserved, err)
	}
	assertFloorLoggedOnce(t, logs)
}

// The shared allocation path refuses too, so no caller can bypass the worker.
func TestReleaseFloorRefusesDirectAllocation(t *testing.T) {
	s := newNativeRuntimeServer(t)
	retainDirectory(t, s, runtimeDirectoryUser(true), 1)
	floorSubject(t, s, 1, false)
	if _, err := s.ssoLifecycle.AllocateNativeAccount(context.Background(), s.stateDir, separationIssuer, "native-runtime-one", s.nativeDomains, s.users, nativeMailboxLimits); !errors.Is(err, sso.ErrNativeReleaseFloor) {
		t.Fatal("direct allocation below floor:", err)
	}
	if _, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("allocated below floor: %v", err)
	}
}

// Equal revision and equal activity is the evidence's own state: unchanged
// active subjects keep working after release without a resync. Disagreeing
// evidence at one revision merges to inactive and refuses.
func TestReleaseFloorAcceptsUnchangedActiveRow(t *testing.T) {
	for _, merged := range []bool{false, true} {
		s := newNativeRuntimeServer(t)
		retainDirectory(t, s, runtimeDirectoryUser(true), 1)
		if merged {
			floorSubject(t, s, 1, false)
		}
		floorSubject(t, s, 1, true)
		if err := s.reconcileNativeSubject(context.Background(), separationIssuer, "native-runtime-one"); err != nil {
			t.Fatal(err)
		}
		u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
		if published := err == nil && u.NativeMailboxSource != ""; published == merged {
			t.Fatalf("merged=%v published=%v %v", merged, published, err)
		}
	}
}

// Case C: a published account the repair deactivated is not reallocated from
// a pending active row, and its assignment stays where it was.
func TestReleaseFloorKeepsRepairedAccountInactive(t *testing.T) {
	s, logs := newFloorServer(t)
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
	workerPasses(t, s)
	after, _, err := s.ssoLifecycle.NativeAssignment(separationIssuer, "native-runtime-one")
	if err != nil || after.Revision != before.Revision || after.Status != before.Status || after.DesiredActive != before.DesiredActive {
		t.Fatalf("floored assignment changed: %+v -> %+v %v", before, after, err)
	}
	if current, err := s.users.Get(u.ID); err != nil || current.Active {
		t.Fatal("floored account reactivated", err)
	}
	assertFloorLoggedOnce(t, logs)
}

// The webhook refuses a stale activation (422) but applies a deactivation
// still queued upstream when the floor was written.
func TestReleaseFloorWebhook(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "floor-create", 1, runtimeDirectoryUser(true)))
	floorSubject(t, s, 2, false)
	if w := postDirectory(t, s, testSyncKey, "user.updated", "floor-stale-active", 2, runtimeDirectoryUser(true)); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stale activation: %d %s", w.Code, w.Body.String())
	}
	if got := directoryStatus(t, postDirectory(t, s, testSyncKey, "user.deleted", "floor-delete", 2, runtimeDirectoryUser(false))); got != sso.DirectoryApplied {
		t.Fatal(got)
	}
	if u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one"); err != nil || u.Active {
		t.Fatal("queued deletion not applied", err)
	}
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
