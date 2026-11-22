package sso

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The org purge's ledger step (tenancy design K10, "Purge (`deleted`)" step 4). Multi-tenant
// only: nothing here runs in a single-tenant install.

// PurgeOwner is one subject whose ledger rows a purge scrubs.
type PurgeOwner struct{ Issuer, Subject string }

// purgeTriggers are the never-delete triggers the purge's named exception suspends. They are
// dropped and recreated (SQLite) or disabled and enabled (PostgreSQL) inside the purge's own
// write transaction, so every other connection, and the exact-schema check after the
// commit, sees them unchanged.
var purgeTriggers = []string{"mailboxes_no_delete", "addresses_no_delete", "address_history_no_delete"}

var purgeTriggerTable = map[string]string{"mailboxes_no_delete": "mailboxes", "addresses_no_delete": "addresses", "address_history_no_delete": "address_history"}

// triggerDDL is name's CREATE TRIGGER statement exactly as authoritySchemaV3 spells it.
func triggerDDL(name string) string {
	return regexp.MustCompile(`(?s)CREATE TRIGGER ` + name + ` .*?END;`).FindString(authoritySchemaV3)
}

// ErrNoNativeLedger is a fresh install's answer: no ledger, so nothing native to purge.
var ErrNoNativeLedger = errors.New("sso: no native ledger")

// v3 runs fn on the v3 database (SQLite or PostgreSQL) in one transaction. Writers hold
// the directory fence. Any other format than v3 or none is ErrNativeMigration.
func (s *LifecycleStore) v3(ctx context.Context, write bool, fn func(*sqliteTx) error) error {
	if format, err := detectAuthority(filepath.Dir(s.path)); err != nil {
		return err
	} else if format == authorityFresh {
		return ErrNoNativeLedger
	}
	if write {
		release, err := s.LockDirectoryContext(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	b, err := storeAuthority{s}.backend(ctx, write)
	if err != nil {
		return err
	}
	db, ok := b.(sqliteAuthority)
	if !ok {
		return ErrNativeMigration
	}
	_, err = db.run(ctx, write, nil, fn)
	return err
}

// NativePurgeMailboxes lists the owners' mailboxes, primary and extra.
func (s *LifecycleStore) NativePurgeMailboxes(ctx context.Context, owners []PurgeOwner) ([]string, error) {
	var out []string
	err := s.v3(ctx, false, func(t *sqliteTx) error {
		var err error
		out, err = ownerMailboxes(t, owners)
		return err
	})
	return out, err
}

func ownerMailboxes(t *sqliteTx, owners []PurgeOwner) ([]string, error) {
	var out []string
	for _, o := range owners {
		r := authorityRows{}
		if err := queryRows(t.ctx, t.c, "m", "SELECT id FROM mailboxes WHERE issuer = ? AND subject = ? ORDER BY id", r, o.Issuer, o.Subject); err != nil {
			return nil, err
		}
		for _, row := range r["m"] {
			id, _ := row[0].(string)
			out = append(out, id)
		}
	}
	return out, nil
}

// PurgeNative scrubs the owners from the ledger under the named trigger exception (K open
// question 9). Inside one write transaction it first calls live, which must return nil (a
// live org_purges row), then tomb with every address the owners' mailboxes ever held
// (current rows and history), which must record their tombstones. Then:
//   - those addresses, their history and pending routes are deleted;
//   - extra mailboxes are deleted (the ledger refuses an extra mailbox without its primary
//     address); primary mailboxes stay as disabled tombstones;
//   - accounts stay inactive; directory rows stay as the revision fence, inactive and
//     without their resource; primary-email rows are deleted.
func (s *LifecycleStore) PurgeNative(ctx context.Context, owners []PurgeOwner, live func() error, tomb func([]string) error) error {
	return s.v3(ctx, true, func(t *sqliteTx) error {
		if err := live(); err != nil {
			return err
		}
		mailboxes, err := ownerMailboxes(t, owners)
		if err != nil {
			return err
		}
		var addresses []string
		seen := map[string]bool{}
		for _, id := range mailboxes {
			r := authorityRows{}
			if err := queryRows(ctx, t.c, "a", "SELECT address FROM addresses WHERE mailbox = ? UNION SELECT address FROM address_history WHERE mailbox = ?", r, id, id); err != nil {
				return err
			}
			for _, row := range r["a"] {
				if a, _ := row[0].(string); !seen[a] {
					seen[a] = true
					addresses = append(addresses, a)
				}
			}
		}
		if err := tomb(addresses); err != nil {
			return err
		}
		exec := func(q string, args ...any) error {
			_, err := t.c.ExecContext(ctx, q, args...)
			return err
		}
		for _, name := range purgeTriggers {
			q := "DROP TRIGGER " + name
			if t.postgres {
				q = "ALTER TABLE " + purgeTriggerTable[name] + " DISABLE TRIGGER " + name
			}
			if err := exec(q); err != nil {
				return err
			}
		}
		for _, a := range addresses {
			for _, q := range []string{"DELETE FROM address_history WHERE address = ?", "DELETE FROM addresses WHERE address = ?", "DELETE FROM routes_pending WHERE address = ?"} {
				if err := exec(q, a); err != nil {
					return err
				}
			}
		}
		for _, id := range mailboxes {
			for _, q := range []string{"DELETE FROM mailboxes WHERE id = ? AND kind = 'extra'", "UPDATE mailboxes SET state = 'disabled' WHERE id = ?"} {
				if err := exec(q, id); err != nil {
					return err
				}
			}
		}
		for _, o := range owners {
			for _, q := range []string{"UPDATE accounts SET desired_active = 0 WHERE issuer = ? AND subject = ?",
				"UPDATE directory SET resource = NULL, active = 0 WHERE issuer = ? AND subject = ?",
				"DELETE FROM directory_primary_emails WHERE issuer = ? AND subject = ?"} {
				if err := exec(q, o.Issuer, o.Subject); err != nil {
					return err
				}
			}
		}
		for _, name := range purgeTriggers {
			q := triggerDDL(name)
			if t.postgres {
				q = "ALTER TABLE " + purgeTriggerTable[name] + " ENABLE TRIGGER " + name
			}
			if q == "" {
				return errors.New("sso: purge trigger " + name + " is not in the schema")
			}
			if err := exec(q); err != nil {
				return err
			}
		}
		// The Cloudflare routing table republishes on this mark.
		n, err := t.Change()
		if err != nil {
			return err
		}
		return exec("UPDATE meta SET value = ? WHERE key = 'change'", strconv.FormatInt(n+1, 10))
	})
}

// NativePurgeLeft counts what PurgeNative should have removed and did not, for the owners
// and the snapshot's mailboxes: an address or history row of a mailbox, an extra or
// undisabled mailbox, an active account, a directory row with a resource or active, and a
// primary-email row.
func (s *LifecycleStore) NativePurgeLeft(ctx context.Context, owners []PurgeOwner, mailboxes []string) (int, error) {
	total := 0
	err := s.v3(ctx, false, func(t *sqliteTx) error {
		count := func(q string, args ...any) error {
			row, _, err := t.row(q, args...)
			if err == nil {
				n, _ := row[0].(int64)
				total += int(n)
			}
			return err
		}
		for _, id := range mailboxes {
			if err := count("SELECT (SELECT count(*) FROM addresses WHERE mailbox = ?) + (SELECT count(*) FROM address_history WHERE mailbox = ?) + (SELECT count(*) FROM mailboxes WHERE id = ? AND (kind = 'extra' OR state <> 'disabled'))", id, id, id); err != nil {
				return err
			}
		}
		for _, o := range owners {
			if err := count("SELECT (SELECT count(*) FROM accounts WHERE issuer = ? AND subject = ? AND desired_active <> 0) + (SELECT count(*) FROM directory WHERE issuer = ? AND subject = ? AND (resource IS NOT NULL OR active <> 0)) + (SELECT count(*) FROM directory_primary_emails WHERE issuer = ? AND subject = ?) + (SELECT count(*) FROM mailboxes WHERE issuer = ? AND subject = ? AND (kind = 'extra' OR state <> 'disabled'))",
				o.Issuer, o.Subject, o.Issuer, o.Subject, o.Issuer, o.Subject, o.Issuer, o.Subject); err != nil {
				return err
			}
		}
		return nil
	})
	return total, err
}

// tombstoned refuses an address the purge tombstoned (K10): the multi-mode conflict check
// consults it before the ledger. A single-tenant store has no hook.
func (s *LifecycleStore) tombstoned(address string) error {
	if s.Tombstoned == nil {
		return nil
	}
	gone, err := s.Tombstoned(strings.ToLower(address))
	if err == nil && gone {
		err = ErrNativeAddressConflict
	}
	return err
}
