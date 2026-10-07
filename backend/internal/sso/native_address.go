package sso

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

var (
	ErrNativeAddressInvalid       = errors.New("address must be a bare ASCII dot-atom address")
	ErrNativeAddressUnknown       = errors.New("mailbox or address not found")
	ErrNativeAddressConflict      = errors.New("address is taken, reserved, a primary address or not in the required state")
	ErrNativeAddressAdministrator = errors.New("administrator identities own no aliases; use the everyday identity's mailbox")
	ErrNativeAddressDomain        = errors.New("address domain is not a configured mail domain")
	ErrNativeAddressDirectory     = errors.New("address is a KyIdentity primary address")
)

// NativeAddress is one ledger address. It locates ownership, not permission:
// only an active address routes or may be used as From.
type NativeAddress struct {
	Address    string `json:"address"`
	Mailbox    string `json:"mailbox"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Generation int64  `json:"generation"`
}

// NativeMailbox lists a mailbox and its addresses for administrators. User is
// the owning account's ID (its primary mailbox ID).
type NativeMailbox struct {
	ID        string          `json:"mailbox"`
	User      string          `json:"user"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Addresses []NativeAddress `json:"addresses"`
}

func (x nativeLedgerAddress) public(address string) NativeAddress {
	return NativeAddress{Address: address, Mailbox: x.Mailbox, Kind: x.Kind, State: x.State, Generation: x.Generation}
}

// NativeAddresses reads every ledger address. Callers that act on the answer
// hold the directory lock.
func (s *LifecycleStore) NativeAddresses() (map[string]NativeAddress, error) {
	f, err := s.loadNative()
	if err != nil {
		return nil, err
	}
	addresses := make(map[string]NativeAddress, len(f.stored.Addresses))
	for address, x := range f.stored.Addresses {
		addresses[address] = x.public(address)
	}
	return addresses, nil
}

// NativeMailboxes lists mailboxes, sorted, with their addresses sorted.
func (s *LifecycleStore) NativeMailboxes() ([]NativeMailbox, error) {
	f, err := s.loadNative()
	if err != nil {
		return nil, err
	}
	mailboxes := []NativeMailbox{}
	for _, id := range slices.Sorted(maps.Keys(f.stored.Mailboxes)) {
		m := f.stored.Mailboxes[id]
		box := NativeMailbox{ID: id, User: f.stored.Accounts[directoryKey(m.Owner.Issuer, m.Owner.Subject)].PrimaryMailbox, Kind: m.Kind, State: m.State, Addresses: []NativeAddress{}}
		for _, address := range slices.Sorted(maps.Keys(f.stored.Addresses)) {
			if x := f.stored.Addresses[address]; x.Mailbox == id {
				box.Addresses = append(box.Addresses, x.public(address))
			}
		}
		mailboxes = append(mailboxes, box)
	}
	return mailboxes, nil
}

// ReconcileNativeAddresses is the worker's level-triggered pass. It commits
// only when an address state diverged from the rule (for example after a
// retired domain is re-added), so a healthy ledger is not rewritten; it always
// retries the inactive routes a failed commit left pending.
func (s *LifecycleStore) ReconcileNativeAddresses(ctx context.Context) error {
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	f, err := s.loadNative()
	if err != nil || len(f.Accounts) == 0 {
		return err
	}
	lifecycle, err := s.load()
	if err != nil {
		return err
	}
	l := f.ledger(lifecycle.Directory)
	before := maps.Clone(l.Addresses)
	inactive, err := s.syncAddressStates(&l, lifecycle.Directory)
	if err != nil {
		return err
	}
	if maps.EqualFunc(before, l.Addresses, func(a, b nativeLedgerAddress) bool { return a.State == b.State && a.Generation == b.Generation }) {
		return deactivateRoutes(l, inactive)
	}
	return s.commitNative(f, nil)
}

// AddNativeAlias gives an everyday identity's mailbox a new address. Records
// are never deleted, so an address the ledger has ever held is refused; a
// released one comes back only through ReassignNativeAddress.
func (s *LifecycleStore) AddNativeAlias(ctx context.Context, stateRoot, mailboxID, address string) (NativeAddress, error) {
	return s.changeNativeAddress(ctx, stateRoot, address, func(f nativeAssignments, domains NativeDomainSet, directory map[string]DirectoryState, x nativeLedgerAddress, exists bool) (nativeLedgerAddress, error) {
		if exists {
			return x, ErrNativeAddressConflict
		}
		if err := f.stored.aliasOwner(mailboxID, address, directory); err != nil {
			return x, err
		}
		x = nativeLedgerAddress{Mailbox: mailboxID, Kind: "alias", State: "disabled", Generation: 1, History: []nativeAddressHistory{{Mailbox: mailboxID, Generation: 1}}}
		if f.stored.desiredActive(address, mailboxID, directory, domains) {
			x.State = "active"
		}
		return x, nil
	})
}

// ReleaseNativeAlias reserves an alias: generation+1 and its route inactive
// in the same action. It stays reserved until deliberately reassigned.
func (s *LifecycleStore) ReleaseNativeAlias(ctx context.Context, stateRoot, address string) (NativeAddress, error) {
	return s.changeNativeAddress(ctx, stateRoot, address, func(_ nativeAssignments, _ NativeDomainSet, _ map[string]DirectoryState, x nativeLedgerAddress, exists bool) (nativeLedgerAddress, error) {
		if !exists {
			return x, ErrNativeAddressUnknown
		}
		if x.Kind != "alias" || x.State == "reserved" {
			return x, ErrNativeAddressConflict
		}
		x.State = "reserved"
		x.Generation++
		return x, nil
	})
}

// ReassignNativeAddress hands a reserved alias to a mailbox (its former one
// included) at a new generation, so mail frozen against any earlier
// generation never matches it.
func (s *LifecycleStore) ReassignNativeAddress(ctx context.Context, stateRoot, address, mailboxID string) (NativeAddress, error) {
	return s.changeNativeAddress(ctx, stateRoot, address, func(f nativeAssignments, domains NativeDomainSet, directory map[string]DirectoryState, x nativeLedgerAddress, exists bool) (nativeLedgerAddress, error) {
		if !exists {
			return x, ErrNativeAddressUnknown
		}
		if x.Kind != "alias" || x.State != "reserved" {
			return x, ErrNativeAddressConflict
		}
		if err := f.stored.aliasOwner(mailboxID, address, directory); err != nil {
			return x, err
		}
		x.Generation++
		x.Mailbox = mailboxID
		x.History = append(slices.Clone(x.History), nativeAddressHistory{Mailbox: mailboxID, Generation: x.Generation})
		x.State = "disabled"
		if f.stored.desiredActive(address, mailboxID, directory, domains) {
			x.State = "active"
		}
		return x, nil
	})
}

// aliasOwner admits only an everyday identity's existing mailbox, and never an
// address any KyIdentity resource names as its primary: that subject may not
// have a ledger record yet, and its provisioning must not fail.
func (l nativeLedger) aliasOwner(mailboxID, address string, directory map[string]DirectoryState) error {
	m, ok := l.Mailboxes[mailboxID]
	if !ok {
		return ErrNativeAddressUnknown
	}
	key := directoryKey(m.Owner.Issuer, m.Owner.Subject)
	return everydayOwner(l.Accounts[key], directory[key], address, directory)
}

// everydayOwner admits an everyday (non-administrator, non-legacyMixedUse)
// owner and refuses an address any KyIdentity resource names as its primary.
func everydayOwner(a nativeLedgerAccount, d DirectoryState, address string, directory map[string]DirectoryState) error {
	if d.Resource == nil || HasAdminRole(d.Resource.Roles) || a.LegacyMixedUse {
		return ErrNativeAddressAdministrator
	}
	for _, d := range directory {
		if d.Resource == nil {
			continue
		}
		for _, email := range d.Resource.Emails {
			if email.Primary && strings.EqualFold(strings.TrimSpace(email.Value), address) {
				return ErrNativeAddressDirectory
			}
		}
	}
	return nil
}

// changeNativeAddress holds domain -> directory locks (the retirement order)
// so the domain check and the ledger write are one decision.
func (s *LifecycleStore) changeNativeAddress(ctx context.Context, stateRoot, address string, change func(nativeAssignments, NativeDomainSet, map[string]DirectoryState, nativeLedgerAddress, bool) (nativeLedgerAddress, error)) (NativeAddress, error) {
	if !strings.Contains(address, "@") {
		return NativeAddress{}, ErrNativeAddressInvalid
	}
	address, err := nativeAddress(address, AddressDomain(address))
	if err != nil || !nativeDomain(AddressDomain(address)) {
		return NativeAddress{}, ErrNativeAddressInvalid
	}
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return NativeAddress{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := fsutil.LockFileContext(ctx, filepath.Join(filepath.Dir(s.path), NativeDomainsFile))
	if err != nil {
		return NativeAddress{}, err
	}
	defer release()
	releaseDirectory, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return NativeAddress{}, err
	}
	defer releaseDirectory()
	domains, err := NewNativeDomainStore(filepath.Dir(s.path)).ReadSet()
	if err != nil {
		return NativeAddress{}, err
	}
	f, err := s.loadNative()
	if err != nil {
		return NativeAddress{}, err
	}
	lifecycle, err := s.load()
	if err != nil {
		return NativeAddress{}, err
	}
	x, exists := f.stored.Addresses[address]
	x, err = change(f, domains, lifecycle.Directory, x, exists)
	if err != nil {
		return NativeAddress{}, err
	}
	// Release needs no domain: retired domains must still be able to shed aliases.
	if _, configured := domains.Domains[AddressDomain(address)]; !configured && x.State != "reserved" {
		return NativeAddress{}, ErrNativeAddressDomain
	}
	f.stored.Addresses[address] = x
	if err := s.commitNative(f, nil); err != nil {
		if errors.Is(err, ErrNativeRoutesPending) {
			return x.public(address), err
		}
		return NativeAddress{}, err
	}
	return x.public(address), nil
}
