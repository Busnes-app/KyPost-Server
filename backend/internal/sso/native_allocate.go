package sso

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// AllocateNativeAccount is an internal, disabled new-account publication flow.
// DNS happens before locks. Lock order: domain -> directory -> users -> account.
// The prepared mailbox and acknowledged assignment exist before users.json can
// expose this ID to lazy state creators. Existing IMAP accounts are never adopted.
func (s *LifecycleStore) AllocateNativeAccount(ctx context.Context, stateRoot, issuer, subject string, domains *NativeDomainStore, accounts *users.Store, limits mailbox.Limits) (users.User, error) {
	if stateRoot == "" || domains == nil || accounts == nil {
		return users.User{}, ErrNativeProvisioning
	}
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return users.User{}, err
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return users.User{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proof, err := domains.Verify(ctx)
	if err != nil {
		return users.User{}, err
	}
	if proof.Issuer != issuer {
		return users.User{}, ErrNativeDomain
	}
	ctx, proofCancel := context.WithDeadline(ctx, time.Unix(proof.VerifiedUntil, 0))
	defer proofCancel()
	release, err := fsutil.LockFileContext(ctx, domains.path)
	if err != nil {
		return users.User{}, err
	}
	defer release()
	current, err := domains.Read()
	if err != nil {
		return users.User{}, err
	}
	if current != proof || proof.VerifiedUntil <= time.Now().Unix() {
		return users.User{}, ErrNativeDomain
	}
	releaseDirectory, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return users.User{}, err
	}
	defer releaseDirectory()
	d, found, err := s.Directory(issuer, subject)
	if err != nil {
		return users.User{}, err
	}
	if !found || !d.Active || d.Resource == nil || d.Resource.Active == nil || !*d.Resource.Active || d.Resource.ID != subject {
		return users.User{}, ErrNativeProvisioning
	}
	f, err := s.loadNative()
	if err != nil {
		return users.User{}, err
	}
	id := f.Accounts[directoryKey(issuer, subject)].Owner.Mailbox
	if id == "" {
		id, err = fsutil.NewUUIDv4()
		if err != nil {
			return users.User{}, err
		}
	}
	role := users.RoleUser
	if HasAdminRole(d.Resource.Roles) {
		role = users.RoleAdmin
	}
	username := d.Resource.UserName
	if users.ValidateUsername(username) != nil {
		username = "native-" + id
	}
	prepare := func() (string, error) {
		a, e := s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, id, proof.Domain, limits, func(root string, o mailbox.Owner, address string, l mailbox.Limits) (string, error) {
			return mailbox.PrepareAccountContext(ctx, root, o, address, l)
		})
		return a.Source, e
	}
	user, err := accounts.PublishPreparedSSOUser(ctx, id, username, role, issuer, subject, d.Resource.UserName, d.Resource.Email(), prepare)
	if errors.Is(err, users.ErrUsernameTaken) {
		return accounts.PublishPreparedSSOUser(ctx, id, "native-"+id, role, issuer, subject, d.Resource.UserName, d.Resource.Email(), prepare)
	}
	return user, err
}
