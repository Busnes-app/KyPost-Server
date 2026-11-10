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

// nativeUserStorageForAdmission uses the initialization fence from this call's
// current lifecycle bytes. Storage and format checks still run on every call.
func (s *LifecycleStore) nativeUserStorageForAdmission(stateRoot string, u users.User, initialized bool, held *mailbox.Prepared) (NativeAssignment, error) {
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return NativeAssignment{}, err
	}
	root, err := nativeUserRoot(stateRoot, u)
	if err != nil {
		return NativeAssignment{}, err
	}
	// Decode the same ledger read the format check accepted, not a second read.
	ledger, exists, err := currentNativeFormat(filepath.Dir(s.path))
	if err != nil {
		return NativeAssignment{}, err
	}
	a, ok, err := nativeAssignmentFromLedger(ledger, exists, directoryKey(u.NativeMailboxIssuer, u.SSOSub), initialized)
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
	ledger, exists, err := currentNativeFormat(filepath.Dir(s.path))
	if err != nil {
		return NativeAssignment{}, err
	}
	key := directoryKey(u.NativeMailboxIssuer, u.SSOSub)
	// Reuse decoded contents only after reading this call's current bytes.
	// Ownership validation grants no access and still checks prepared storage.
	// The lifecycle supplies only the initialization flag. Decoding the ledger
	// read above is safe because persistNative sets the flag before writing
	// any ledger and never clears it; keep that order.
	_, _, initialized, err := s.nativeDirectoryForAdmission(key)
	if err != nil {
		return NativeAssignment{}, err
	}
	a, ok, err := nativeAssignmentFromLedger(ledger, exists, key, initialized)
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
