package sso

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// ReserveHRAddress assigns an address before an HR-sourced identity exists.
// This reserves ownership only: no directory grant, account, storage or live route.
// Normal signed directory provisioning prepares the mailbox after activation.
func (s *LifecycleStore) ReserveHRAddress(ctx context.Context, root, issuer, subject, preferred, domain string, domains *NativeDomainStore, limits mailbox.Limits) (string, error) {
	if root == "" || !directoryIdentifier(issuer) || !directoryIdentifier(subject) || !fsutil.SafePathComponent(subject) || !nativeDomain(domain) || len(preferred) > 48 {
		return "", ErrNativeProvisioning
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	preferred = strings.ToLower(preferred)
	if strings.Contains(preferred, "+") {
		return "", ErrNativeProvisioning
	}
	if _, err := nativeAddress(preferred+"@"+domain, domain); err != nil {
		return "", err
	}
	releaseDomain, err := fsutil.LockFileContext(ctx, domains.path)
	if err != nil {
		return "", err
	}
	defer releaseDomain()
	set, err := domains.ReadSet()
	if err != nil || set.Issuer != issuer {
		return "", ErrNativeDomain
	}
	if _, ok := set.Domains[domain]; !ok {
		return "", ErrNativeAddressDomain
	}
	release, err := s.LockDirectoryContext(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := RequireNativeRestoreReleased(root); err != nil {
		return "", err
	}
	var address string
	err = s.authority().Update(ctx, func(w authorityWriter) error {
		f, directory, err := w.Snapshot()
		if err != nil {
			return err
		}
		key := directoryKey(issuer, subject)
		if d, ok := directory[key]; ok {
			// HR reservations never adopt an existing directory identity.
			if _, reserved := f.Accounts[key]; !reserved || d.Resource == nil || HasAdminRole(d.Resource.Roles) {
				return ErrNativeProvisioning
			}
		}
		if a, ok := f.Accounts[key]; ok {
			if a.StateRoot != root || a.Address == "" || AddressDomain(a.Address) != domain {
				return ErrNativeProvisioning
			}
			address = a.Address
			return nil
		}
		// ponytail: bounded suffix search; use an indexed allocator if imports
		// ever exhaust 1,000 identical preferred names in a single domain.
		for suffix := 1; suffix <= 1000; suffix++ {
			local := preferred
			if suffix > 1 {
				local += fmt.Sprintf(".%d", suffix)
			}
			candidate := local + "@" + domain
			if _, taken := f.stored.Addresses[candidate]; taken {
				continue
			}
			clash := false
			for _, d := range directory {
				if d.Resource != nil {
					for _, email := range d.Resource.Emails {
						if email.Primary && strings.EqualFold(strings.TrimSpace(email.Value), candidate) {
							clash = true
						}
					}
				}
			}
			if clash {
				continue
			}
			id, err := fsutil.NewUUIDv4()
			if err != nil {
				return err
			}
			f.Accounts[key] = NativeAssignment{Owner: mailbox.Owner{Issuer: issuer, Subject: subject, Mailbox: id}, Address: candidate, StateRoot: root, Limits: limits, Status: "pending", Failure: "hr_address_reserved"}
			if err := s.writeNative(w, f, nil, true); err != nil {
				return err
			}
			address = candidate
			return nil
		}
		return ErrNativeAddressConflict
	})
	return address, err
}

// The explicit marker distinguishes address-only reservations from damaged
// provisioning evidence. A reservation has never authorized an account or storage.
func (a NativeAssignment) hrReservation() bool {
	return a.Revision == 0 && a.Digest == "" && a.Status == "pending" && a.Failure == "hr_address_reserved" && a.Source == "" && !a.DesiredActive && !a.LegacyMixedUse && a.Address != ""
}
