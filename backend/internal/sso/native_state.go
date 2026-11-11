package sso

import (
	"context"
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
	_, err := s.nativeUserStorage(stateRoot, u)
	return err
}

// nativeUserStorage returns the assignment this call validated. A caller already
// holding the directory fence can reuse it without reading the ledger twice;
// it grants no authority to a later operation.
func (s *LifecycleStore) nativeUserStorage(stateRoot string, u users.User) (NativeAssignment, error) {
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		return NativeAssignment{}, nil
	}
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return NativeAssignment{}, err
	}
	return s.nativeUserOwnership(stateRoot, u)
}

// ValidateNativeUserOwnership permits only ownership validation without a hold
// release. Credential revocation uses this; ordinary state access must use
// ValidateNativeUserStorage so held mail remains inaccessible.
func (s *LifecycleStore) ValidateNativeUserOwnership(stateRoot string, u users.User) error {
	_, err := s.nativeUserOwnership(stateRoot, u)
	return err
}

// nativeUserStorageForAdmission reads the assignment in the caller's View,
// whose initialization flag comes from the same lifecycle read as its
// directory entry. Storage and format checks still run on every call.
func (s *LifecycleStore) nativeUserStorageForAdmission(stateRoot string, u users.User, r authorityReader, held *mailbox.Prepared) (NativeAssignment, error) {
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return NativeAssignment{}, err
	}
	root, err := nativeUserRoot(stateRoot, u)
	if err != nil {
		return NativeAssignment{}, err
	}
	// One ledger read: the format check's bytes are the ones decoded.
	a, ok, err := r.Account(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		return NativeAssignment{}, err
	}
	return validateNativeUserAssignment(root, u, a, ok, held)
}

func (s *LifecycleStore) nativeUserOwnership(stateRoot string, u users.User) (NativeAssignment, error) {
	root, err := nativeUserRoot(stateRoot, u)
	if err != nil {
		return NativeAssignment{}, err
	}
	// Reuse decoded contents only after reading this call's current bytes.
	// Ownership validation grants no access and still checks prepared storage.
	// Account reads the ledger before the lifecycle, which supplies only the
	// initialization flag. That is safe because every ledger write starts with
	// SetInitialized, which writes the flag first, and nothing clears it.
	var a NativeAssignment
	var ok bool
	err = s.authority().View(context.Background(), func(r authorityReader) error {
		a, ok, err = r.Account(u.NativeMailboxIssuer, u.SSOSub)
		return err
	})
	if err != nil {
		return NativeAssignment{}, err
	}
	return validateNativeUserAssignment(root, u, a, ok, nil)
}

func nativeUserRoot(stateRoot string, u users.User) (string, error) {
	if !fsutil.SafePathComponent(u.ID) || u.NativeMailboxIssuer == "" || u.NativeMailboxSource == "" || u.SSOSub == "" {
		return "", ErrNativeProvisioning
	}
	return filepath.Abs(stateRoot)
}

// held, when non-nil, is an open client's storage; see mailbox.Prepared.
func validateNativeUserAssignment(root string, u users.User, a NativeAssignment, ok bool, held *mailbox.Prepared) (NativeAssignment, error) {
	if !ok || a.Owner != (mailbox.Owner{Issuer: u.NativeMailboxIssuer, Subject: u.SSOSub, Mailbox: u.ID}) || a.Source != u.NativeMailboxSource || a.StateRoot != root {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	source, err := held.ValidatePreparedMailbox(filepath.Join(root, "users"), a.Owner, a.Address, a.Limits)
	if err != nil {
		return NativeAssignment{}, err
	}
	if source != a.Source {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	return a, nil
}
