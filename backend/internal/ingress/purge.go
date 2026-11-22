package ingress

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// The org purge's receiving steps (tenancy design, "Purge (`deleted`)" step 2). Multi-tenant
// only.

// PurgeMailboxes deletes the mailboxes' routes and bindings in one transaction. A
// delivery bound only to these mailboxes is archived as discarded, so its bytes go and an
// exact replay is still recognised (ScrubArchived later drops its recipients); a delivery
// shared with another mailbox loses only these bindings.
func (s *Store) PurgeMailboxes(ctx context.Context, mailboxes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	type key struct{ gateway, id string }
	bound := map[key][]string{}
	for _, m := range mailboxes {
		rows, err := tx.QueryContext(ctx, "SELECT b.gateway,b.id,a.mailbox FROM bindings b JOIN bindings a ON a.gateway=b.gateway AND a.id=b.id WHERE b.mailbox=?", m)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k key
			var other string
			if err := rows.Scan(&k.gateway, &k.id, &other); err != nil {
				rows.Close()
				return err
			}
			bound[k] = append(bound[k], other)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	for k, owners := range bound {
		if !slices.ContainsFunc(owners, func(m string) bool { return !slices.Contains(mailboxes, m) }) {
			if err := archive(ctx, tx, s.postgres, "INSERT", "discarded", "gateway=? AND id=?", k.gateway, k.id); err != nil {
				return err
			}
		}
	}
	for _, m := range mailboxes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM bindings WHERE mailbox=?", m); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM routes WHERE mailbox=?", m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ScrubArchived removes every recipient gone reports from the tombstones' recipient lists
// (purge step 4, after the addresses are tombstoned). The tombstone itself stays.
// ponytail: scans every tombstone per purge; an index by recipient is the upgrade if
// purges get slow.
func (s *Store) ScrubArchived(ctx context.Context, gone func(address string) (bool, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	type row struct{ gateway, id, recipients string }
	var changed []row
	if err := eachArchived(ctx, tx, func(gateway, id, recipients string) error {
		kept, err := keep(recipients, gone)
		if err == nil && kept != recipients {
			changed = append(changed, row{gateway, id, kept})
		}
		return err
	}); err != nil {
		return err
	}
	for _, r := range changed {
		if _, err := tx.ExecContext(ctx, "UPDATE archived SET recipients=? WHERE gateway=? AND id=?", r.recipients, r.gateway, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeLeft counts the mailboxes' routes and bindings, and archived recipients gone reports.
func (s *Store) PurgeLeft(ctx context.Context, mailboxes []string, gone func(address string) (bool, error)) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	total := 0
	for _, m := range mailboxes {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM bindings WHERE mailbox=?) + (SELECT count(*) FROM routes WHERE mailbox=?)", m, m).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	err = eachArchived(ctx, tx, func(_, _, recipients string) error {
		kept, err := keep(recipients, gone)
		total += len(splitRecipients(recipients)) - len(splitRecipients(kept))
		return err
	})
	return total, err
}

func eachArchived(ctx context.Context, tx *sql.Tx, fn func(gateway, id, recipients string) error) error {
	rows, err := tx.QueryContext(ctx, "SELECT gateway,id,recipients FROM archived")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var gateway, id, recipients string
		if err := rows.Scan(&gateway, &id, &recipients); err != nil {
			return err
		}
		if err := fn(gateway, id, recipients); err != nil {
			return err
		}
	}
	return rows.Err()
}

func splitRecipients(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func keep(recipients string, gone func(string) (bool, error)) (string, error) {
	list := splitRecipients(recipients)
	var err error
	list = slices.DeleteFunc(list, func(a string) bool {
		g, e := gone(a)
		if e != nil {
			err = e
		}
		return g
	})
	return strings.Join(list, "\n"), err
}
