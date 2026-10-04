package sso

import (
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// ValidateNativeUserStorage guards ordinary state openers and cache hits.
// It verifies immutable storage ownership, not live access: administrative
// revocation must still reach inactive users. Mail admission checks access
// separately under the directory fence before performing mail operations.
func (s *LifecycleStore) ValidateNativeUserStorage(stateRoot string, u users.User) error {
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		return nil
	}
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return err
	}
	if !fsutil.SafePathComponent(u.ID) || u.NativeMailboxIssuer == "" || u.NativeMailboxSource == "" || u.SSOSub == "" {
		return ErrNativeProvisioning
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return err
	}
	a, ok, err := s.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		return err
	}
	if !ok || a.Owner != (mailbox.Owner{Issuer: u.NativeMailboxIssuer, Subject: u.SSOSub, Mailbox: u.ID}) || a.Source != u.NativeMailboxSource || a.StateRoot != root {
		return ErrNativeProvisioning
	}
	source, err := mailbox.ValidatePreparedAccount(root, a.Owner, a.Address, a.Limits)
	if err != nil {
		return err
	}
	if source != a.Source {
		return ErrNativeProvisioning
	}
	return nil
}
