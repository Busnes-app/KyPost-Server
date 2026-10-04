package sso

import (
	"context"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// WithCurrentSettings serializes a local authority commit with Save in every
// process. Load uses atomic file reads without a process mutex, so callers
// holding directory/users fences cannot create a settings-lock inversion.
// The callback must not reenter settings mutations or perform network I/O.
func (s *Store) WithCurrentSettings(ctx context.Context, action func(SSOSettings) error) error {
	if action == nil {
		return ErrNativeProvisioning
	}
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	s.loads.Add(1)
	return action(s.loadUnlocked())
}
