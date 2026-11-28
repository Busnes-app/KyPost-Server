package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// Multi-mode import (receiving tier R8): a pending delivery is one obligation per bound
// mailbox. The caller (app) leases the mailbox, writes its copy with ImportMailbox, and
// runs Complete once its own write has committed. Single mode keeps Import's
// all-or-nothing claim; these functions never take that claim.

// Obligation is one pending delivery bound to a mailbox, in arrival order.
type Obligation struct {
	Sequence int64
	Gateway  string
	ID       string
}

// PendingGateways lists every gateway with pending deliveries, whatever its pin state:
// mail a host committed before its pin was revoked or expired still imports.
func (s *Store) PendingGateways(ctx context.Context) ([]string, error) {
	return s.strings(ctx, "SELECT DISTINCT gateway FROM deliveries WHERE state='pending' ORDER BY gateway")
}

// PendingMailboxes lists every mailbox bound to a pending delivery.
// ponytail: scans bindings, unindexed by mailbox and bounded by the record limit (RX open
// question 4).
func (s *Store) PendingMailboxes(ctx context.Context) ([]string, error) {
	return s.strings(ctx, "SELECT DISTINCT b.mailbox FROM bindings b JOIN deliveries d ON d.gateway=b.gateway AND d.id=b.id WHERE d.state='pending' ORDER BY b.mailbox")
}

func (s *Store) strings(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Obligations pages mailbox's pending deliveries, across gateways, in arrival order (the
// delivery's first bind) after sequence after.
func (s *Store) Obligations(ctx context.Context, mailbox string, after int64, limit int) ([]Obligation, error) {
	if !identifier(mailbox) || after < 0 || limit < 1 || limit > 100 {
		return nil, ErrConflict
	}
	rows, err := s.db.QueryContext(ctx, "SELECT d.rowid,d.gateway,d.id FROM deliveries d WHERE d.state='pending' AND d.rowid>? AND EXISTS(SELECT 1 FROM bindings b WHERE b.gateway=d.gateway AND b.id=d.id AND b.mailbox=?) ORDER BY d.rowid LIMIT ?", after, mailbox, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Obligation
	for rows.Next() {
		var o Obligation
		if err := rows.Scan(&o.Sequence, &o.Gateway, &o.ID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Bindings reads a delivery's bindings without its bytes.
func (s *Store) Bindings(ctx context.Context, gateway, id string) ([]Binding, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return readBindings(ctx, tx, gateway, id)
}

// ImportMailbox commits store's one copy of pending delivery d: one receipt for every
// binding of store's mailbox. d is the caller's Get of the delivery. A repeat finds the
// receipt and writes nothing. It takes no claim and archives nothing (Complete does).
func (s *Store) ImportMailbox(ctx context.Context, d Delivery, folder string, store *mailbox.Store) error {
	if d.State != "pending" {
		return ErrLease
	}
	hash := sha256.Sum256(d.Raw)
	if hex.EncodeToString(hash[:]) != d.Digest {
		return ErrConflict
	}
	owner := store.Owner()
	var recipients []mailbox.Recipient
	for _, b := range d.Bindings {
		if b.Mailbox != owner.Mailbox {
			continue
		}
		if (mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}) != owner {
			return mailbox.ErrOwner
		}
		recipients = append(recipients, mailbox.Recipient{Address: b.Address, Generation: b.Generation})
	}
	if len(recipients) == 0 {
		return ErrRoute
	}
	_, err := store.Import(ctx, mailbox.Receipt{Gateway: d.Gateway, Delivery: d.ID, Sender: d.Sender, Recipients: recipients, Folder: folder}, bytes.NewReader(d.Raw))
	return err
}

// Done is one bound mailbox's obligation state, as Complete's caller read it after its
// own write committed.
type Done int

const (
	NotDone  Done = iota
	Imported      // the mailbox holds the receipt
	Retired       // the binding is recorded retired and can never import
)

// Complete ends a pending delivery whose every binding's mailbox is done in done: it is
// archived as imported when none is retired, and otherwise quarantined keeping only its
// retired bindings, for staff to release or discard. Anything else changes nothing and
// returns the delivery's state. One writer transaction, so of two callers finishing the
// last two obligations at once, one ends it and the other finds it ended.
func (s *Store) Complete(ctx context.Context, gateway, id string, done map[string]Done) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var until int64
	err = tx.QueryRowContext(ctx, "SELECT state,lease_until FROM deliveries WHERE gateway=? AND id=?", gateway, id).Scan(&state, &until)
	if errors.Is(err, sql.ErrNoRows) { // archived or purged already
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// A live claim is single mode's whole-delivery importer: leave it to acknowledge.
	if state != "pending" || until > time.Now().Unix() {
		return state, nil
	}
	bindings, err := readBindings(ctx, tx, gateway, id)
	if err != nil {
		return "", err
	}
	retired := false
	for _, b := range bindings {
		switch done[b.Mailbox] {
		case NotDone:
			return state, nil
		case Retired:
			retired = true
		}
	}
	if len(bindings) == 0 {
		return state, nil
	}
	if !retired {
		if err := archive(ctx, tx, s.postgres, "INSERT", "imported", "gateway=? AND id=? AND state='pending'", gateway, id); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		s.scrub(ctx)
		return "archived", nil
	}
	for _, b := range bindings {
		if done[b.Mailbox] == Imported {
			if _, err := tx.ExecContext(ctx, "DELETE FROM bindings WHERE gateway=? AND id=? AND address=?", gateway, id, b.Address); err != nil {
				return "", err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE deliveries SET state='quarantined',lease='',lease_until=0 WHERE gateway=? AND id=?", gateway, id); err != nil {
		return "", err
	}
	return "quarantined", tx.Commit()
}
