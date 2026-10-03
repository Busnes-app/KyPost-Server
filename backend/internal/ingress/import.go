package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// Import commits one local copy per frozen owner before releasing holding bytes.
// resolve opens that verified owner, never today's email-address destination.
// Callers own stores and their lifetimes. Partial failures resume via receipts.
// No production directory or scheduler invokes this internal bridge yet.
func (s *Store) Import(ctx context.Context, gateway, id string, resolve func(mailbox.Owner) (*mailbox.Store, error)) error {
	d, err := s.Claim(ctx, gateway, id, 5*time.Minute)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(d.Raw)
	if hex.EncodeToString(hash[:]) != d.Digest {
		return ErrConflict
	}
	groups := map[mailbox.Owner][]mailbox.Recipient{}
	for _, b := range d.Bindings {
		owner := mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}
		groups[owner] = append(groups[owner], mailbox.Recipient{Address: b.Address, Generation: b.Generation})
	}
	// Stable order makes recovery reproducible; multiple aliases get one copy.
	owners := make([]mailbox.Owner, 0, len(groups))
	for owner := range groups {
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool {
		a, b := owners[i], owners[j]
		if a.Issuer != b.Issuer {
			return a.Issuer < b.Issuer
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Mailbox < b.Mailbox
	})
	for _, owner := range owners {
		store, err := resolve(owner)
		if err != nil {
			return err
		}
		if store == nil {
			return errors.New("mailbox provisioning unavailable; retain holding copy and reconcile")
		}
		if store.Owner() != owner {
			return mailbox.ErrOwner
		}
		receipt := mailbox.Receipt{Gateway: d.Gateway, Delivery: d.ID, Sender: d.Sender, Recipients: groups[owner]}
		if _, err = store.Import(ctx, receipt, bytes.NewReader(d.Raw)); err != nil {
			return err
		}
	}
	return s.Acknowledge(ctx, d.Gateway, d.ID, d.Lease, d.Digest)
}
