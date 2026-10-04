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
