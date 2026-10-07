package sso

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
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

// AdmitNativeMailbox is AdmitNativeMail for one of the user's mailboxes:
// mailboxID equal to the user ID is the primary. Unknown, foreign and
// disabled mailboxes refuse alike with ErrNativeMailboxUnknown.
func (s *LifecycleStore) AdmitNativeMailbox(ctx context.Context, stateRoot, issuer, userID, mailboxID string, accounts *users.Store) (NativeAssignment, error) {
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
	return s.admitNativeMailbox(ctx, stateRoot, issuer, u, mailboxID)
}

// WithNativeMailAccess holds coherent directory and user authority through a
// local receive/import commit. The action must not re-enter either authority
// store or perform network I/O. Partial mailbox commits recover via receipts.
// mailboxIDs name primary or extra mailboxes; each owner is admitted.
func (s *LifecycleStore) WithNativeMailAccess(ctx context.Context, stateRoot, issuer string, accounts *users.Store, mailboxIDs []string, action func(map[string]NativeAssignment) error) error {
	if len(mailboxIDs) == 0 || len(mailboxIDs) > 100 || action == nil {
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
		f, err := s.loadNative()
		if err != nil {
			return err
		}
		assignments := make(map[string]NativeAssignment, len(mailboxIDs))
		for _, id := range mailboxIDs {
			m, _, ok := f.mailbox(id)
			owner := f.Accounts[directoryKey(m.Owner.Issuer, m.Owner.Subject)].Owner.Mailbox
			u, found := byID[owner]
			if !ok || !found {
				return ErrNativeProvisioning
			}
			a, err := s.admitNativeMailbox(ctx, stateRoot, issuer, u, id)
			if err != nil {
				return err
			}
			assignments[id] = a
		}
		return action(assignments)
	})
}

// admitNativeMailbox admits the owner, then the mailbox: the primary when
// mailboxID is the user's ID, else an extra one owned by the same subject,
// not administrator-disabled, with its storage intact.
func (s *LifecycleStore) admitNativeMailbox(ctx context.Context, stateRoot, issuer string, u users.User, mailboxID string) (NativeAssignment, error) {
	a, err := s.admitNativeMailUser(ctx, stateRoot, issuer, u)
	if err != nil || mailboxID == u.ID {
		return a, err
	}
	f, err := s.loadNative()
	if err != nil {
		return NativeAssignment{}, err
	}
	m, record, ok := f.mailbox(mailboxID)
	if !ok || record.State != "active" || m.Owner.Issuer != issuer || m.Owner.Subject != u.SSOSub || m.Source == "" || m.StateRoot != a.StateRoot {
		return NativeAssignment{}, ErrNativeMailboxUnknown
	}
	source, err := mailbox.ValidatePreparedMailbox(filepath.Join(m.StateRoot, nativeMailboxesDir), m.Owner, m.Address, m.Limits)
	if err != nil {
		return NativeAssignment{}, err
	}
	if source != m.Source {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	return m, ctx.Err()
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
		return NativeAssignment{}, ErrNativeAdministrator
	}
	// The primary address may sit on any configured, non-retired domain.
	domains, err := NewNativeDomainStore(filepath.Dir(s.path)).ReadSet()
	domain := AddressDomain(a.Address)
	if _, configured := domains.Domains[domain]; err != nil || domains.Issuer != issuer || !configured {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	address, err := nativePrimary(*d.Resource, domain)
	if err != nil || address != a.Address || a.Revision > d.Revision {
		return NativeAssignment{}, ErrNativeProvisioning
	}
	return a, ctx.Err()
}
