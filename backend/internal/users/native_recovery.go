package users

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// NativeAccountRepair changes only access on an already published native owner.
// The caller must verify fresh signed recovery evidence and mailbox storage.
type NativeAccountRepair struct {
	ID, Subject, Source string
	Active              bool
	Role                Role
}

// RepairNativeAccounts commits one bounded native-only batch. beforeWrite runs
// under the fresh users fences, after validation and epoch calculation, and must
// durably record the recovery intent/barriers before returning success. Its returned
// release keeps additional commit fences through the users write (also on failure).
// It must not mutate either snapshot, reenter this Store or perform network I/O. Settings
// and directory fences precede this call; session/credential cleanup follows it.
// This method neither validates recovery evidence nor releases a restore hold.
func (s *Store) RepairNativeAccounts(ctx context.Context, issuer string, repairs []NativeAccountRepair, beforeWrite func(current, repaired []User) (release func(), err error)) error {
	if issuer == "" || len(repairs) == 0 || len(repairs) > 256 || beforeWrite == nil {
		return errors.New("bounded native recovery plan and durable intent are required")
	}
	if err := s.lockContext(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	f, err := s.readFileUnlocked()
	if err != nil {
		return err
	}
	if f.Version != 1 {
		return errors.New("unsupported account authority version")
	}
	current := append([]User(nil), f.Users...)
	byID := make(map[string]int, len(f.Users))
	bySubject := make(map[string]int, len(f.Users))
	for i, u := range f.Users {
		if _, exists := byID[u.ID]; exists || u.ID == "" {
			return ErrNativeAccountConflict
		}
		byID[u.ID] = i
		if u.SSOSub != "" {
			if _, exists := bySubject[u.SSOSub]; exists {
				return ErrNativeAccountConflict
			}
			bySubject[u.SSOSub] = i
		}
	}
	seen := make(map[string]bool, len(repairs))
	now := time.Now().UTC().Format(time.RFC3339)
	for _, repair := range repairs {
		i, exists := byID[repair.ID]
		if !exists || seen[repair.ID] || repair.Subject == "" || repair.Source == "" || (repair.Role != RoleAdmin && repair.Role != RoleUser) {
			return ErrNativeAccountConflict
		}
		seen[repair.ID] = true
		u := &f.Users[i]
		if u.NativeMailboxIssuer != issuer || u.NativeMailboxSource != repair.Source || u.SSOSub != repair.Subject {
			return ErrNativeAccountConflict
		}
		if u.Active == repair.Active && u.Role == repair.Role {
			continue
		}
		if u.Active != repair.Active {
			u.DeactivatedAt = ""
			if !repair.Active {
				u.DeactivatedAt = now
			}
		}
		u.Active, u.Role, u.UpdatedAt = repair.Active, repair.Role, now
	}
	// Never leave a repaired instance without an active administrator. The API
	// separately requires a usable legacy recovery administrator while held.
	if FirstAdminFrom(f.Users).ID == "" {
		return ErrLastActiveAdmin
	}
	if err = s.advanceNativeSendEpochs(&f); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	releaseCommit, err := beforeWrite(current, f.Users)
	if releaseCommit != nil {
		defer releaseCommit()
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return s.writeFileUnlocked(f)
}

// DeactivateForRestoreRelease runs one native restore release under the users
// fences. prepare sees the current accounts, must durably record the release
// intent and returns the non-native accounts to deactivate and to demote to
// user, plus a release for commit fences it holds. These are written before
// commit runs, so a failed commit leaves them changed: it never activates or
// promotes anything.
func (s *Store) DeactivateForRestoreRelease(ctx context.Context, prepare func(current []User) (deactivate, demote []string, release func(), err error), commit func() error) error {
	if err := s.lockContext(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	f, err := s.readFileUnlocked()
	if err != nil {
		return err
	}
	if f.Version != 1 {
		return errors.New("unsupported account authority version")
	}
	deactivate, demote, releaseCommit, err := prepare(append([]User(nil), f.Users...))
	if releaseCommit != nil {
		defer releaseCommit()
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	change := func(id string, apply func(*User) bool) error {
		i := slices.IndexFunc(f.Users, func(u User) bool { return u.ID == id })
		if i < 0 || !f.Users[i].Active || f.Users[i].NativeMailboxIssuer != "" || f.Users[i].NativeMailboxSource != "" || !apply(&f.Users[i]) {
			return ErrNativeAccountConflict
		}
		f.Users[i].UpdatedAt = now
		return nil
	}
	for _, id := range deactivate {
		if err = change(id, func(u *User) bool { u.Active, u.DeactivatedAt = false, now; return true }); err != nil {
			return err
		}
	}
	for _, id := range demote {
		if err = change(id, func(u *User) bool { ok := u.Role == RoleAdmin; u.Role = RoleUser; return ok }); err != nil {
			return err
		}
	}
	if FirstAdminFrom(f.Users).ID == "" {
		return ErrLastActiveAdmin
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if len(deactivate)+len(demote) != 0 {
		if err = s.writeFileUnlocked(f); err != nil {
			return err
		}
	}
	return commit()
}
