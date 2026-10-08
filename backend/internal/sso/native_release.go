package sso

import (
	"errors"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// NativeReleaseFloor is the signed evidence a restore hold release consumed
// for one subject. Active directory state below Revision, or at it when the
// evidence was inactive, predates or contradicts that evidence, so it cannot
// provision, allocate or reactivate. Newer revisions and inactive state pass.
type NativeReleaseFloor struct {
	Revision int64 `json:"revision"`
	Active   bool  `json:"active"`
}

var ErrNativeReleaseFloor = errors.New("native subject is at or below its restore release floor; only a newer KyIdentity revision (resync) can provision or reactivate it")

// RecordNativeReleaseFloors raises floors monotonically: a lower revision never
// replaces a higher one, and disagreeing evidence at one revision keeps inactive.
func (s *LifecycleStore) RecordNativeReleaseFloors(issuer string, floors map[string]NativeReleaseFloor) error {
	return fsutil.WithFileLock(s.path, func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		if err = f.raiseReleaseFloors(issuer, floors); err != nil {
			return err
		}
		return fsutil.PersistJSONFile(s.path, f)
	})
}

func (f *lifecycleFile) raiseReleaseFloors(issuer string, floors map[string]NativeReleaseFloor) error {
	if !directoryIdentifier(issuer) {
		return ErrNativeRecovery
	}
	if f.ReleaseFloors == nil {
		f.ReleaseFloors = map[string]NativeReleaseFloor{}
	}
	for subject, floor := range floors {
		if !nativeRecoveryIdentifier(subject) || floor.Revision <= 0 {
			return ErrNativeRecovery
		}
		k := directoryKey(issuer, subject)
		prior, ok := f.ReleaseFloors[k]
		switch {
		case !ok || floor.Revision > prior.Revision:
			f.ReleaseFloors[k] = floor
		case floor.Revision == prior.Revision:
			prior.Active = prior.Active && floor.Active
			f.ReleaseFloors[k] = prior
		}
	}
	return nil
}

// refuses reports whether active state at revision predates the evidence.
// KyIdentity bumps the revision on every desired-state change, so the evidence
// revision with the evidence's own activity is that state, not a stale one.
// Inactive state always passes: it can only fail closed.
func (floor NativeReleaseFloor) refuses(revision int64, active bool) bool {
	return active && (revision < floor.Revision || revision == floor.Revision && !floor.Active)
}

// CheckNativeReleaseFloor refuses provisioning from retained directory state
// the release evidence superseded; the disable path always passes.
func (s *LifecycleStore) CheckNativeReleaseFloor(issuer, subject string, d DirectoryState) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	if floor, ok := f.ReleaseFloors[directoryKey(issuer, subject)]; ok && floor.refuses(d.Revision, d.Active) {
		return ErrNativeReleaseFloor
	}
	return nil
}
