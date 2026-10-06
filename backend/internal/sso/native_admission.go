package sso

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// AdmitNativeMail reads current authority before an operation, never permission
// cached with a client. Directory updates serialize with this decision; an
// operation already admitted may finish after later offboarding.
func (s *LifecycleStore) AdmitNativeMail(ctx context.Context, stateRoot, issuer, userID string, accounts *users.Store) (NativeAssignment, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return NativeAssignment{}, err
	}
	defer release()
	u, err := accounts.Get(userID)
	if err != nil {
		return NativeAssignment{}, err
	}
	return s.admitNativeMailUser(ctx, stateRoot, issuer, u)
}

// WithNativeMailAccess holds coherent directory and user authority through a
// local receive/import commit. The action must not re-enter either authority
// store or perform network I/O. Partial mailbox commits recover via receipts.
func (s *LifecycleStore) WithNativeMailAccess(ctx context.Context, stateRoot, issuer string, accounts *users.Store, userIDs []string, action func(map[string]NativeAssignment) error) error {
	if len(userIDs) == 0 || len(userIDs) > 100 || action == nil {
		return ErrNativeProvisioning
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.LockDirectoryContext(ctx)
	if err != nil {
		return err
	}
	defer release()
	return accounts.WithCurrentUsers(ctx, func(current []users.User) error {
		byID := make(map[string]users.User, len(current))
		for _, u := range current {
			byID[u.ID] = u
		}
		assignments := make(map[string]NativeAssignment, len(userIDs))
		for _, id := range userIDs {
			u, found := byID[id]
			if !found {
				return ErrNativeProvisioning
			}
			a, err := s.admitNativeMailUser(ctx, stateRoot, issuer, u)
			if err != nil {
				return err
			}
			assignments[id] = a
		}
		return action(assignments)
	})
}

// Caller holds the directory fence and supplies a current user snapshot.
func (s *LifecycleStore) admitNativeMailUser(ctx context.Context, stateRoot, issuer string, u users.User) (NativeAssignment, error) {
	if issuer == "" || u.NativeMailboxIssuer != issuer || u.NativeMailboxSource == "" || !u.Active || u.SSOLinkRevoked() {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	d, known, err := s.Directory(issuer, u.SSOSub)
	if err != nil {
		return NativeAssignment{}, err
	}
	if !known || !d.Active || d.Resource == nil || d.Resource.Active == nil || !*d.Resource.Active || d.Resource.ID != u.SSOSub {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	revision, err := d.Resource.Revision("user.updated")
	if err != nil || revision != d.Revision {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	role := users.RoleUser
	if HasAdminRole(d.Resource.Roles) {
		role = users.RoleAdmin
	}
	if u.Role != role {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	if err := s.ValidateNativeUserStorage(stateRoot, u); err != nil {
		return NativeAssignment{}, err
	}
	a, _, err := s.NativeAssignment(issuer, u.SSOSub)
	if err != nil {
		return NativeAssignment{}, err
	}
	// Administrators get no mailbox; promotion refuses from the next request.
	if role == users.RoleAdmin && !a.LegacyMixedUse {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	domain, err := NewNativeDomainStore(filepath.Dir(s.path)).Read()
	if err != nil || domain.Issuer != issuer {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	address, err := nativePrimary(*d.Resource, domain.Domain)
	if err != nil || address != a.Address || a.Revision > d.Revision {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	return a, ctx.Err()
}
