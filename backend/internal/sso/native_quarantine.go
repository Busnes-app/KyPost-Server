package sso

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// ErrQuarantineRelease is release's refusal when a frozen mailbox cannot take
// the delivery any more. Discard stays available.
var ErrQuarantineRelease = errors.New("release refused: a frozen mailbox was deleted or disabled, changed owner, or its owner or storage is no longer admitted; discard remains available")

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
	out := make([]QuarantinedDelivery, 0, len(rows))
	for _, q := range rows {
		d := QuarantinedDelivery{Sequence: q.Sequence, Gateway: q.Gateway, ID: q.ID, Sender: q.Sender, ReceivedAt: q.Received, Size: q.Size, Recipients: []QuarantinedRecipient{}}
		for _, b := range q.Bindings {
			r := QuarantinedRecipient{Address: b.Address, Mailbox: b.Mailbox, Generation: b.Generation}
			a, found, err := s.NativeMailboxAssignment(b.Mailbox)
			if err != nil {
				return nil, err
			}
			if found && a.Owner == (mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}) {
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
// then, and reassignment is the usual reason it was quarantined. All owners or
// none; a repeated release of a released delivery succeeds.
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
	for _, b := range d.Bindings {
		owner := mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}
		if stores[owner] != nil {
			continue
		}
		a, found, err := s.NativeMailboxAssignment(owner.Mailbox)
		if err != nil {
			return err
		}
		if !found || a.Owner != owner || a.Source == "" {
			return ErrQuarantineRelease
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
	if errors.Is(err, ErrNativeProvisioning) || errors.Is(err, ErrNativeMailboxUnknown) {
		return ErrQuarantineRelease
	}
	return err
}
