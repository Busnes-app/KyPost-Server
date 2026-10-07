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
			if a, _, found := f.mailbox(b.Mailbox); found && a.Owner == (mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}) {
				r.User = a.UserID()
			}
			d.Recipients = append(d.Recipients, r)
		}
		out = append(out, d)
	}
	return out, nil
}

// ReleaseQuarantined delivers a quarantined delivery to the mailboxes it was
// frozen to, never to an address's current owner. Each frozen mailbox must
// still exist, be active, belong to the same issuer/subject and have admitted
// storage, checked under the directory and users fences Import holds. The
// address generation is not checked: the mail was addressed to that mailbox
// then, and reassignment is the usual reason it was quarantined. Admission is
// all owners or none; a capacity or crash failure during the commits can leave
// some owners with the mail, which a retry completes without duplicates. A
// repeated release of a released delivery succeeds.
func (s *LifecycleStore) ReleaseQuarantined(ctx context.Context, stateRoot, issuer string, accounts *users.Store, holding *ingress.Store, gateway, id string) error {
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
	for _, b := range d.Bindings {
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
		return holding.Release(ctx, gateway, id, func(owner mailbox.Owner) (*mailbox.Store, error) {
			if stores[owner] == nil {
				return nil, ErrQuarantineRelease
			}
			return stores[owner], nil
		})
	})
	if errors.Is(err, ErrNativeProvisioning) || errors.Is(err, ErrNativeMailboxUnknown) || errors.Is(err, ErrQuarantineRelease) {
		return refused()
	}
	return err
}
