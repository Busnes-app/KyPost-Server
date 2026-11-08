//go:build linux

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeRepairDiscoveryKeepsFreshRepairAndLocalRevocation(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "repair-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer(separationIssuer, "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	// Retain a newer resource without doing the publication follow-on. This
	// models interruption after directory acknowledgment, before storage repair.
	resource := runtimeDirectoryUser(true)
	resource["meta"] = map[string]any{"version": `W/"2"`}
	raw, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	var desired sso.DirectoryUser
	if err := json.Unmarshal(raw, &desired); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "repair-retained", Type: "user.updated", At: time.Now()}
	if _, err := s.ssoLifecycle.ApplyDirectoryUser(separationIssuer, ev, desired, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	subjects, err := s.ssoLifecycle.NativeDirectoryRepairSubjects(context.Background(), separationIssuer, s.users)
	if err != nil || len(subjects) != 1 {
		t.Fatal(subjects, err)
	}
	// A discovery result must not bypass a hold that appears before repair.
	hold := filepath.Join(s.stateDir, sso.NativeRestoreHoldFile)
	if err := os.WriteFile(hold, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, subjects[0]); err == nil {
		t.Fatal("discovery authorized held repair")
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileNativeSubject(context.Background(), separationIssuer, subjects[0]); err != nil {
		t.Fatal(err)
	}
	if subjects, err = s.ssoLifecycle.NativeDirectoryRepairSubjects(context.Background(), separationIssuer, s.users); err != nil || len(subjects) != 0 {
		t.Fatal(subjects, err)
	}
	if _, err := s.users.Deactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	if subjects, err = s.ssoLifecycle.NativeDirectoryRepairSubjects(context.Background(), separationIssuer, s.users); err != nil || len(subjects) != 0 {
		t.Fatal("local revocation became repair work", subjects, err)
	}
	if _, _, err := s.nativeMailAssignment(context.Background(), u.ID); err == nil {
		t.Fatal("deactivated user admitted")
	}
}
