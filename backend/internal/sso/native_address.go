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
	ID    string `json:"mailbox"`
	User  string `json:"user"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Prepared is false while a creation has reserved the mailbox but not yet
	// published its storage; nothing may poll, send from or open it.
	Prepared  bool            `json:"prepared"`
	Addresses []NativeAddress `json:"addresses"`
	// QuotaBytes is the mailbox's storage quota; Dir its storage directory.
	QuotaBytes int64  `json:"quotaBytes"`
	Dir        string `json:"-"`
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

// NativeAddress reads one ledger address, the zero value when it has none.
// Callers that act on the answer hold the directory lock.
func (s *LifecycleStore) NativeAddress(address string) (a NativeAddress, err error) {
	err = s.authority().View(context.Background(), func(r authorityReader) error {
		x, ok, err := r.Address(address)
		if ok {
			a = x.public(address)
		}
		return err
	})
	if err != nil {
		return NativeAddress{}, err
	}
	return a, nil
}

// NativeMailboxes lists mailboxes, sorted, with their addresses sorted.
func (s *LifecycleStore) NativeMailboxes() ([]NativeMailbox, error) {
	f, err := s.loadNative()
	if err != nil {
		return nil, err
	}
	return nativeMailboxes(f, func(string) bool { return true }), nil
}

// NativeMailboxesWithoutAddresses is NativeMailboxes with Addresses nil, read
// without the address rows or a whole-ledger decode: discovery for the outbox
// and extra-mailbox polling. It grants nothing; every operation admits afresh.
func (s *LifecycleStore) NativeMailboxesWithoutAddresses() (mailboxes []NativeMailbox, err error) {
	err = s.authority().View(context.Background(), func(r authorityReader) error {
		mailboxes, err = r.Mailboxes()
		return err
	})
	if err != nil {
		return nil, err
	}
	return mailboxes, nil
}

// NativeMailboxesOf is NativeMailboxes for one owning account's mailboxes,
// read by owner (OwnerMailboxes), not from the whole ledger.
func (s *LifecycleStore) NativeMailboxesOf(userID string) (mailboxes []NativeMailbox, err error) {
	err = s.authority().View(context.Background(), func(r authorityReader) error {
		f, err := r.OwnerMailboxes(userID)
		mailboxes = nativeMailboxes(f, func(user string) bool { return user == userID })
		return err
	})
	if err != nil {
		return nil, err
	}
	return mailboxes, nil
}

// nativeMailboxes lists the mailboxes of f whose owning user keep accepts.
func nativeMailboxes(f nativeAssignments, keep func(user string) bool) []NativeMailbox {
	// One pass over the sorted addresses groups them by mailbox, in order.
	addresses := map[string][]NativeAddress{}
	for _, address := range slices.Sorted(maps.Keys(f.stored.Addresses)) {
		x := f.stored.Addresses[address]
		addresses[x.Mailbox] = append(addresses[x.Mailbox], x.public(address))
	}
	mailboxes := []NativeMailbox{}
	for _, id := range slices.Sorted(maps.Keys(f.stored.Mailboxes)) {
		m := f.stored.Mailboxes[id]
		box := NativeMailbox{ID: id, User: f.stored.Accounts[directoryKey(m.Owner.Issuer, m.Owner.Subject)].PrimaryMailbox, Kind: m.Kind, State: m.State, Prepared: m.Source != "", Addresses: append([]NativeAddress{}, addresses[id]...), QuotaBytes: m.Limits.PayloadBytes}
		if !keep(box.User) {
			continue
		}
		if a, _, ok := f.mailboxStorage(id); ok {
			box.Dir = a.Dir(a.StateRoot)
		}
		mailboxes = append(mailboxes, box)
	}
	return mailboxes
}

// ReconcileNativeAddresses is the worker's level-triggered pass. It first
// validates the whole store, then commits only when an address state diverged
// from the rule (for example after a retired domain is re-added), so a
// healthy ledger is not rewritten; it always retries routes_pending.
func (s *LifecycleStore) ReconcileNativeAddresses(ctx context.Context) error {
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	return s.authority().Update(ctx, func(w authorityWriter) error {
		// Whole-store validation (owner decision Q5): a failure refuses this
		// pass's writes only; the worker logs it.
		if err := w.Check(); err != nil {
			return err
		}
		f, directory, err := w.Snapshot()
		if err != nil || len(f.Accounts) == 0 {
			return err
		}
		l := f.ledger(directory)
		before := maps.Clone(l.Addresses)
		inactive, err := s.syncAddressStates(&l, directory)
		if err != nil {
			return err
		}
		if maps.EqualFunc(before, l.Addresses, func(a, b nativeLedgerAddress) bool { return a.State == b.State && a.Generation == b.Generation }) {
			return w.MarkRoutesPending(inactive)
		}
		return s.writeNative(w, f, nil, true)
	})
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
	var f nativeAssignments
	var directory map[string]DirectoryState
	err = s.authority().View(ctx, func(r authorityReader) error {
		f, directory, err = r.Snapshot()
		return err
	})
	if err != nil {
		return NativeAddress{}, err
	}
	x, exists := f.stored.Addresses[address]
	x, err = change(f, domains, directory, x, exists)
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
