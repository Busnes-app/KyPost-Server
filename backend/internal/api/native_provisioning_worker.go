package api

import (
	"context"
	"errors"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Initial bounded profile: no automatic eviction. Mail-sized backup/capacity
// qualification precedes raising these persisted per-account limits.
var nativeMailboxLimits = mailbox.Limits{MessageBytes: 5 << 20, PayloadBytes: 32 << 20, Records: 10000}

func (s *Server) reconcileNativeSubject(ctx context.Context, issuer, subject string) error {
	if err := sso.RequireNativeRestoreReleased(s.stateDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.ssoLifecycle.LockDirectoryContext(ctx)
	if err != nil {
		return err
	}
	d, known, err := s.ssoLifecycle.Directory(issuer, subject)
	if err != nil || !known || d.Resource == nil {
		release()
		return sso.ErrNativeProvisioning
	}
	settings := s.ssoStore.Load()
	if !settings.Enabled || settings.IssuerURL != issuer {
		release()
		return sso.ErrNativeProvisioning
	}
	u, lookupErr := s.users.GetBySSOSubIssuer(issuer, subject)
	if lookupErr != nil && !errors.Is(lookupErr, users.ErrNotFound) {
		release()
		return lookupErr
	}
	if lookupErr == nil && u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		release()
		return nil // existing IMAP and administrator accounts never migrate through directory repair
	}
	// Only the disable path may act on state the release evidence superseded.
	if err := s.ssoLifecycle.CheckNativeReleaseFloor(issuer, subject, d); err != nil {
		release()
		return err
	}
	if lookupErr != nil && d.Active && sso.HasAdminRole(d.Resource.Roles) {
		defer release()
		return s.provisionDirectoryUser(*d.Resource, users.RoleAdmin)
	}
	a, reserved, err := s.ssoLifecycle.NativeAssignment(issuer, subject)
	if err != nil {
		release()
		return err
	}
	// Retained revisions already applied access changes before persistence.
	// Storage retries must not replay those changes over local revocation.
	if lookupErr == nil && reserved && a.Status == "applied" && a.Revision == d.Revision && a.Digest == d.Digest {
		release()
		return nil
	}
	active := d.Active
	release()
	if !active {
		if !reserved {
			return nil
		}
		// Disabling needs no proof; the reserved address's domain may be retired.
		domains, err := s.nativeDomains.ReadSet()
		domain := sso.AddressDomain(a.Address)
		if a.Address == "" {
			domain = domains.Founding
		}
		if err != nil || domains.Issuer != issuer || !domains.Known(domain) {
			return sso.ErrNativeProvisioning
		}
		_, err = s.ssoLifecycle.DisableNativeMailboxContext(ctx, s.stateDir, issuer, subject, a.Owner.Mailbox, domain, a.Limits)
		return err
	}
	limits := nativeMailboxLimits
	if reserved {
		limits = a.Limits
	}
	_, err = s.ssoLifecycle.AllocateNativeAccount(ctx, s.stateDir, issuer, subject, s.nativeDomains, s.users, limits)
	return err
}

// StartNativeProvisioning retries durable desired work, including a restart
// after directory acknowledgment but before account publication. Healthy
// acknowledged accounts do not trigger repeated DNS lookups.
func (s *Server) StartNativeProvisioning(ctx context.Context) {
	if !s.nativeMail {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		settings := s.ssoStore.Load()
		if sso.RequireNativeRestoreReleased(s.stateDir) == nil {
			if err := s.ssoLifecycle.ReconcileNativeAddresses(ctx); err != nil {
				s.logger.Error("native address reconcile pending", "error", err.Error())
			}
		}
		if settings.Enabled {
			subjects, err := s.ssoLifecycle.NativeDirectorySubjects(settings.IssuerURL)
			if err != nil {
				s.logger.Error("native directory repair unavailable", "error", err.Error())
			} else {
				for _, subject := range subjects {
					if ctx.Err() != nil {
						return
					}
					if err := s.reconcileNativeSubject(ctx, settings.IssuerURL, subject); err != nil {
						s.logger.Error("native mailbox repair pending", "error", err.Error())
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
