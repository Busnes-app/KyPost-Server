package sso

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// ErrNativeMailboxUnknown refuses an unknown, foreign or disabled mailbox
// alike, so a caller learns nothing about mailboxes it does not own.
var ErrNativeMailboxUnknown = fmt.Errorf("%w: mailbox not found", ErrNativeProvisioning)

// Extra mailboxes live under $STATE/mailboxes/<mailboxID>, beside (never in)
// $STATE/users, so code that enumerates users is unaffected.
const nativeMailboxesDir = "mailboxes"

// extraMailboxPrefix keeps extra mailbox IDs disjoint from user IDs, which
// are bare UUIDs; primary mailbox IDs equal user IDs.
const extraMailboxPrefix = "mbx-"

// CreateNativeMailbox gives an everyday identity (userID, its primary
// mailbox) an extra mailbox whose primary address the administrator chose.
// The ledger records the mailbox before its storage is prepared and its
// source after, so a retry with the same address resumes an interrupted
// creation; the address routes only once the source is recorded.
func (s *LifecycleStore) CreateNativeMailbox(ctx context.Context, stateRoot, userID, address string) (NativeMailbox, error) {
	address, err := canonicalNativeAddress(address)
	if err != nil {
		return NativeMailbox{}, err
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return NativeMailbox{}, err
	}
	var id string
	var pending error
	err = s.withNativeMailboxes(ctx, stateRoot, func(f nativeAssignments, domains NativeDomainSet, directory map[string]DirectoryState) error {
		if !domains.Domains[AddressDomain(address)].Established {
			return ErrNativeAddressDomain
		}
		key, owner := "", NativeAssignment{}
		for k, a := range f.Accounts {
			if a.Owner.Mailbox == userID {
				key, owner = k, a
			}
		}
		if key == "" {
			return ErrNativeAddressUnknown
		}
		if err := everydayOwner(f.stored.Accounts[key], directory[key], address, directory); err != nil {
			return err
		}
		if owner.Source == "" || owner.StateRoot != root {
			return ErrNativeAddressConflict
		}
		if x, exists := f.stored.Addresses[address]; exists {
			// Only an unfinished creation for the same owner resumes.
			m := f.stored.Mailboxes[x.Mailbox]
			if x.Kind != "primary" || m.Kind != "extra" || m.Source != "" || directoryKey(m.Owner.Issuer, m.Owner.Subject) != key {
				return ErrNativeAddressConflict
			}
			id = x.Mailbox
		} else {
			uuid, err := fsutil.NewUUIDv4()
			if err != nil {
				return err
			}
			id = extraMailboxPrefix + uuid
			m := nativeLedgerMailbox{Kind: "extra", State: "active", StateRoot: owner.StateRoot, Limits: owner.Limits}
			m.Owner.Issuer, m.Owner.Subject = owner.Owner.Issuer, owner.Owner.Subject
			f.stored.Mailboxes[id] = m
			f.stored.Addresses[address] = nativeLedgerAddress{Mailbox: id, Kind: "primary", State: "disabled", Generation: 1, History: []nativeAddressHistory{{Mailbox: id, Generation: 1}}}
			// Reserve durably BEFORE touching storage; a killed writer resumes.
			if err := s.commitNative(f, nil); err != nil && !errors.Is(err, ErrNativeRoutesPending) {
				return err
			}
			if f, err = s.loadNative(); err != nil {
				return err
			}
		}
		m := f.stored.Mailboxes[id]
		source, err := mailbox.PrepareMailboxContext(ctx, filepath.Join(root, nativeMailboxesDir), mailbox.Owner{Issuer: m.Owner.Issuer, Subject: m.Owner.Subject, Mailbox: id}, address, m.Limits)
		if err != nil {
			return err
		}
		m.Source = source
		f.stored.Mailboxes[id] = m
		pending = s.commitNative(f, nil)
		if errors.Is(pending, ErrNativeRoutesPending) {
			return nil
		}
		return pending
	})
	if err != nil {
		return NativeMailbox{}, err
	}
	return s.nativeMailboxListing(id, pending)
}

// SetNativeMailboxState disables or re-enables an extra mailbox. The
// desired-state rule then disables or re-enables its addresses with a
// generation bump; its mail is retained either way.
func (s *LifecycleStore) SetNativeMailboxState(ctx context.Context, stateRoot, mailboxID string, active bool) (NativeMailbox, error) {
	want := "disabled"
	if active {
		want = "active"
	}
	var pending error
	err := s.withNativeMailboxes(ctx, stateRoot, func(f nativeAssignments, _ NativeDomainSet, _ map[string]DirectoryState) error {
		m, ok := f.stored.Mailboxes[mailboxID]
		if !ok || m.Kind != "extra" {
			return ErrNativeAddressUnknown
		}
		if m.State == want || m.Source == "" {
			return ErrNativeAddressConflict
		}
		m.State = want
		f.stored.Mailboxes[mailboxID] = m
		pending = s.commitNative(f, nil)
		if errors.Is(pending, ErrNativeRoutesPending) {
			return nil
		}
		return pending
	})
	if err != nil {
		return NativeMailbox{}, err
	}
	return s.nativeMailboxListing(mailboxID, pending)
}

// withNativeMailboxes holds domain -> directory locks (the retirement order)
// so the domain check and the ledger write are one decision.
func (s *LifecycleStore) withNativeMailboxes(ctx context.Context, stateRoot string, action func(nativeAssignments, NativeDomainSet, map[string]DirectoryState) error) error {
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := fsutil.LockFileContext(ctx, filepath.Join(filepath.Dir(s.path), NativeDomainsFile))
	if err != nil {
		return err
	}
	defer release()
	releaseDirectory, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer releaseDirectory()
	domains, err := NewNativeDomainStore(filepath.Dir(s.path)).ReadSet()
	if err != nil {
		return err
	}
	f, err := s.loadNative()
	if err != nil {
		return err
	}
	lifecycle, err := s.load()
	if err != nil {
		return err
	}
	return action(f, domains, lifecycle.Directory)
}

// nativeMailboxListing reads the committed mailbox back; pending is a
// committed change whose inactive route write is still owed.
func (s *LifecycleStore) nativeMailboxListing(id string, pending error) (NativeMailbox, error) {
	all, err := s.NativeMailboxes()
	if err != nil {
		return NativeMailbox{}, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, pending
		}
	}
	return NativeMailbox{}, ErrNativeProvisioning
}

func canonicalNativeAddress(address string) (string, error) {
	address, err := nativeAddress(address, AddressDomain(address))
	if err != nil || !nativeDomain(AddressDomain(address)) {
		return "", ErrNativeAddressInvalid
	}
	return address, nil
}
