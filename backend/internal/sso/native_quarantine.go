package sso

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// ErrQuarantineRelease is release's refusal because a frozen mailbox cannot
// take the delivery now. ErrQuarantineInactive means a durable decision, so
// discard is the way out; ErrQuarantineUnadmitted may clear by itself.
var (
	ErrQuarantineRelease    = errors.New("release refused")
	ErrQuarantineInactive   = fmt.Errorf("%w: a frozen mailbox was deleted or disabled, or its owner was offboarded, promoted or changed; discard remains available", ErrQuarantineRelease)
	ErrQuarantineUnadmitted = fmt.Errorf("%w: a frozen mailbox is not currently admitted (directory sync, storage or sign-on configuration); resync and retry", ErrQuarantineRelease)
	// Unresolved deliveries were captured under a routing table this server
	// does not know, typically mail that waited at the provider through a
	// restore. Their original owner cannot be proven: equal generations after
	// a restore do not mean the same owner.
	ErrQuarantineUnresolved      = fmt.Errorf("%w: captured under a routing table this server does not know (for example mail waiting through a restore), so its original owner is unknown; release it to the recipient address's current owner with explicit confirmation, or discard it", ErrQuarantineRelease)
	ErrQuarantineResolved        = fmt.Errorf("%w: release to the current owner applies only to deliveries whose original owner is unknown; use an ordinary release", ErrQuarantineRelease)
	ErrQuarantineAddressInactive = fmt.Errorf("%w: the recipient address is not active now, so it has no current owner; discard remains available", ErrQuarantineRelease)
)

// mailboxRetired reports a durable reason the mailbox cannot take mail frozen
// to owner: it is gone or owned by someone else, administrator-disabled, or
// its primary address is no longer active, which the ledger's desired-state
// rule records for an offboarded or promoted owner and a retired domain.
func (f nativeAssignments) mailboxRetired(owner mailbox.Owner) bool {
	a, m, ok := f.mailbox(owner.Mailbox)
	if !ok || a.Owner != owner || m.State == "disabled" {
		return true
	}
	for _, x := range f.stored.Addresses {
		if x.Mailbox == owner.Mailbox && x.Kind == "primary" && x.State != "active" {
			return true
		}
	}
	return false
}

// QuarantinedRecipient is one frozen binding: the address as received and the
// mailbox and owning user it was bound to then.
type QuarantinedRecipient struct {
	Address    string `json:"address"`
	Mailbox    string `json:"mailbox"`
	User       string `json:"user"`
	Generation int64  `json:"generation"`
	// CurrentMailbox and CurrentUser are today's owner of an unresolved
	// recipient's active address: not proven to be the original.
	CurrentMailbox string `json:"currentMailbox,omitempty"`
	CurrentUser    string `json:"currentUser,omitempty"`
}

// QuarantinedDelivery is envelope metadata only: never body, subject or headers.
type QuarantinedDelivery struct {
	Sequence   int64                  `json:"sequence"`
	Gateway    string                 `json:"gateway"`
	ID         string                 `json:"id"`
	Sender     string                 `json:"sender"`
	ReceivedAt time.Time              `json:"receivedAt"`
	Size       int64                  `json:"size"`
	Recipients []QuarantinedRecipient `json:"recipients"`
	// Unresolved: the original owner is unknown; only an explicit release to
	// the current owner or a discard applies.
	Unresolved bool `json:"unresolved"`
}

// QuarantinedDeliveries pages up to 100 quarantined envelopes after sequence.
// User is "" when the frozen mailbox no longer exists.
func (s *LifecycleStore) QuarantinedDeliveries(ctx context.Context, holding *ingress.Store, after int64) ([]QuarantinedDelivery, error) {
	rows, err := holding.ListQuarantined(ctx, after, 100)
	if err != nil {
		return nil, err
	}
	f, err := s.loadNative()
	if err != nil {
		return nil, err
	}
	out := make([]QuarantinedDelivery, 0, len(rows))
	for _, q := range rows {
		d := QuarantinedDelivery{Sequence: q.Sequence, Gateway: q.Gateway, ID: q.ID, Sender: q.Sender, ReceivedAt: q.Received, Size: q.Size, Recipients: []QuarantinedRecipient{}}
		for _, b := range q.Bindings {
			r := QuarantinedRecipient{Address: b.Address, Mailbox: b.Mailbox, Generation: b.Generation}
			if b.Unresolved() {
				d.Unresolved = true
				if c, err := f.currentOwner(b.Address); err == nil {
					r.CurrentMailbox = c.Mailbox
					if a, _, found := f.mailbox(c.Mailbox); found {
						r.CurrentUser = a.UserID()
					}
				}
			} else if a, _, found := f.mailbox(b.Mailbox); found && a.Owner == (mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}) {
				r.User = a.UserID()
			}
			d.Recipients = append(d.Recipients, r)
		}
		out = append(out, d)
	}
	return out, nil
}

// currentOwner is the binding an active address has now: its mailbox's owner
// at the address's current generation.
func (f nativeAssignments) currentOwner(address string) (ingress.Binding, error) {
	x, ok := f.stored.Addresses[address]
	a, _, found := f.mailbox(x.Mailbox)
	if !ok || !found || x.State != "active" {
		return ingress.Binding{}, ErrQuarantineAddressInactive
	}
	return ingress.Binding{Address: address, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Generation: x.Generation}, nil
}

// ReleaseQuarantined delivers a quarantined delivery to the mailboxes it was
// frozen to, never to an address's current owner. The one exception is
// toCurrentOwner, an administrator's explicit choice for an unresolved
// delivery (no known original owner): it binds the recipient to the owner of
// its active address now, at the current generation, under the same fences,
// then releases. Without it an unresolved delivery is refused, and with it a
// resolved one is. Each frozen mailbox must
// still exist, be active, belong to the same issuer/subject and have admitted
// storage, checked under the directory and users fences Import holds. The
// address generation is not checked: the mail was addressed to that mailbox
// then, and reassignment is the usual reason it was quarantined. Admission is
// all owners or none; a capacity or crash failure during the commits can leave
// some owners with the mail, which a retry completes without duplicates. A
// repeated release of a released delivery succeeds.
func (s *LifecycleStore) ReleaseQuarantined(ctx context.Context, stateRoot, issuer string, accounts *users.Store, holding *ingress.Store, gateway, id string, toCurrentOwner bool) error {
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return err
	}
	d, err := holding.Get(ctx, gateway, id)
	if err != nil {
		return err
	}
	if d.State == "archived" && d.Disposition == "released" {
		return nil
	}
	if d.State != "quarantined" {
		return ingress.ErrNotQuarantined
	}
	bindings := d.Bindings
	unresolved := slices.ContainsFunc(bindings, ingress.Binding.Unresolved)
	switch {
	case unresolved && !toCurrentOwner:
		return ErrQuarantineUnresolved
	case !unresolved && toCurrentOwner:
		return ErrQuarantineResolved
	case unresolved:
		f, err := s.loadNative()
		if err != nil {
			return err
		}
		if len(bindings) != 1 {
			return ErrQuarantineRelease
		}
		b, err := f.currentOwner(bindings[0].Address)
		if err != nil {
			return err
		}
		bindings = []ingress.Binding{b}
	}
	stores := map[mailbox.Owner]*mailbox.Store{}
	sources := map[mailbox.Owner]string{}
	defer func() {
		for _, store := range stores {
			_ = store.Close()
		}
	}()
	ids := []string{}
	owners := []mailbox.Owner{}
	// refused names a durable reason when the ledger records one, so a
	// transient refusal never steers the administrator to discard.
	refused := func() error {
		f, err := s.loadNative()
		if err != nil {
			return err
		}
		for _, owner := range owners {
			if f.mailboxRetired(owner) {
				return ErrQuarantineInactive
			}
		}
		return ErrQuarantineUnadmitted
	}
	for _, b := range bindings {
		owner := mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}
		if slices.Contains(owners, owner) {
			continue
		}
		owners = append(owners, owner)
		a, found, err := s.NativeMailboxAssignment(owner.Mailbox)
		if err != nil {
			return err
		}
		if !found || a.Owner != owner || a.Source == "" {
			return refused()
		}
		if int64(len(d.Raw)) > a.Limits.MessageBytes {
			return mailbox.ErrCapacity
		}
		// Opened before the fences, as importDelivery does; the source is
		// re-checked under them.
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(stateRoot), "mailbox"), owner, a.Limits, a.Source)
		if err != nil {
			return err
		}
		stores[owner], sources[owner] = store, a.Source
		ids = append(ids, owner.Mailbox)
	}
	err = s.WithNativeMailAccess(ctx, stateRoot, issuer, accounts, ids, func(current map[string]NativeAssignment) error {
		for owner := range stores {
			if a := current[owner.Mailbox]; a.Owner != owner || a.Source != sources[owner] {
				return ErrQuarantineRelease
			}
		}
		if unresolved {
			// Re-read under the directory fence: the address must still
			// belong to the owner the administrator was shown.
			f, err := s.loadNative()
			if err != nil {
				return err
			}
			if b, err := f.currentOwner(bindings[0].Address); err != nil || b != bindings[0] {
				return ErrQuarantineRelease
			}
			if err := holding.ResolveQuarantined(ctx, gateway, id, bindings[0]); err != nil {
				return err
			}
		}
		return holding.Release(ctx, gateway, id, func(owner mailbox.Owner) (*mailbox.Store, error) {
			if stores[owner] == nil {
				return nil, ErrQuarantineRelease
			}
			return stores[owner], nil
		})
	})
	if errors.Is(err, ErrQuarantineAddressInactive) {
		return err
	}
	if errors.Is(err, ErrNativeProvisioning) || errors.Is(err, ErrNativeMailboxUnknown) || errors.Is(err, ErrQuarantineRelease) {
		return refused()
	}
	return err
}
