package sso

import (
	"errors"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// NativeReleaseFloor is the signed evidence a restore hold release consumed
// for one subject. Retained directory state at or below Revision predates that
// evidence, so it can no longer provision, allocate or reactivate; only a
// strictly newer signed directory event lifts the floor. Active is what the
// evidence said, kept for status and audit; the guard does not trust it.
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

// CheckNativeReleaseFloor refuses active retained directory state at or below
// the subject's release floor: it predates the evidence the release consumed.
// Inactive state passes, so the disable path stays open.
func (s *LifecycleStore) CheckNativeReleaseFloor(issuer, subject string, d DirectoryState) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	if floor, ok := f.ReleaseFloors[directoryKey(issuer, subject)]; ok && d.Active && d.Revision <= floor.Revision {
		return ErrNativeReleaseFloor
	}
	return nil
}
