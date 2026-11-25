package sso

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// WithCurrentSettings serializes an authority commit with Save in every
// process. Load remains lock-free to avoid settings/directory lock inversion.
// The callback must not reenter settings mutations or perform network I/O.
func (s *Store) WithCurrentSettings(ctx context.Context, action func(context.Context, SSOSettings) error) error {
	if action == nil {
		return ErrNativeProvisioning
	}
	if vol.OpenDocument(s.path).IsTable() {
		return s.withSettingsFence(ctx, func(owned context.Context) error {
			s.loads.Add(1)
			var settings SSOSettings
			raw, err := vol.OpenDocument(s.path).Read()
			if errors.Is(err, os.ErrNotExist) {
				settings.AutoProvision = true
			} else if err == nil {
				err = json.Unmarshal(raw, &settings)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return action(owned, settings)
		})
	}
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	s.loads.Add(1)
	return action(ctx, s.loadUnlocked())
}

// The instance store already exists in table mode. Opening it read-only gives
// the fence its logical namespace without creating state or changing authority.
func (s *Store) withSettingsFence(ctx context.Context, action func(context.Context) error) error {
	root, err := state.OpenReadOnly(s.stateDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WithFences(ctx, []state.Fence{{Name: vol.FenceSSO}}, func(owned context.Context) error {
		err := action(owned)
		if err == nil && owned.Err() != nil {
			return context.Cause(owned)
		}
		return err
	})
}
