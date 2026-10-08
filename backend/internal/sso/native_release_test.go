package sso

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

const releaseIssuer = "https://idp.example"

func releaseResource(t *testing.T, revision int64) (DirectoryUser, string) {
	t.Helper()
	active := true
	u := DirectoryUser{Schemas: []string{scimUserSchema}, ID: "subject", ExternalID: "subject", UserName: "subject", Active: &active}
	u.Meta.Version = fmt.Sprintf(`W/"%d"`, revision)
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return u, EventDigest("user.updated", raw)
}

func TestReleaseFloorsAreMonotonicAndDurable(t *testing.T) {
	dir := t.TempDir()
	s := NewLifecycleStore(dir)
	record := func(rev int64, active bool) {
		t.Helper()
		if err := s.RecordNativeReleaseFloors(releaseIssuer, map[string]NativeReleaseFloor{"subject": {Revision: rev, Active: active}}); err != nil {
			t.Fatal(err)
		}
	}
	record(5, true)
	record(5, false) // disagreement at one revision keeps inactive
	record(5, true)
	record(3, true) // lower never replaces
	for _, bad := range []map[string]NativeReleaseFloor{{"subject": {Revision: 0}}, {"bad subject": {Revision: 9}}} {
		if err := s.RecordNativeReleaseFloors(releaseIssuer, bad); !errors.Is(err, ErrNativeRecovery) {
			t.Fatal("invalid floor accepted", bad, err)
		}
	}
	// A fresh store is a restart: the floor lives in sso-lifecycle.json.
	f, err := NewLifecycleStore(dir).load()
	if err != nil {
		t.Fatal(err)
	}
	if got := f.ReleaseFloors[directoryKey(releaseIssuer, "subject")]; got != (NativeReleaseFloor{Revision: 5}) {
		t.Fatalf("floor = %+v", got)
	}
}

// The webhook shares applyDirectory: a revision at or below the floor is a
// conflict even though it is newer than the retained row.
func TestReleaseFloorRefusesStaleDirectoryEvents(t *testing.T) {
	s := NewLifecycleStore(t.TempDir())
	if err := s.RecordNativeReleaseFloors(releaseIssuer, map[string]NativeReleaseFloor{"subject": {Revision: 2}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rev  int64
		want error
	}{{2, ErrDirectoryConflict}, {3, nil}} {
		rev, want := c.rev, c.want
		u, digest := releaseResource(t, rev)
		applied := false
		ev := syncauth.Event{ID: fmt.Sprintf("event-%d", rev), Type: "user.updated", At: time.Now()}
		_, err := s.ApplyDirectoryUser(releaseIssuer, ev, u, digest, func() (bool, error) { applied = true; return false, nil })
		if !errors.Is(err, want) || applied != (want == nil) {
			t.Fatalf("revision %d: applied=%v err=%v", rev, applied, err)
		}
	}
}

// Backups seal sso-lifecycle.json; a corrupt floor must not restore as a
// silently ineffective one.
func TestReleaseFloorSnapshotValidation(t *testing.T) {
	dir := t.TempDir()
	s := NewLifecycleStore(dir)
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNativeReleaseFloors(releaseIssuer, map[string]NativeReleaseFloor{"subject": {Revision: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateNativeSnapshot(state, nil); err != nil {
		t.Fatal("valid floor refused:", err)
	}
	for _, floors := range []map[string]NativeReleaseFloor{
		{directoryKey(releaseIssuer, "subject"): {Revision: 0}},
		{"no-separator": {Revision: 2}},
	} {
		if err := fsutil.PersistJSONFile(s.path, lifecycleFile{ReleaseFloors: floors}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ValidateNativeSnapshot(state, nil); !errors.Is(err, ErrNativeProvisioning) {
			t.Fatal("malformed floor accepted", floors, err)
		}
	}
}
