package state

import (
	"context"
	"database/sql"
	"fmt"
)

// Out-of-office bounds (docs/OUT_OF_OFFICE.md).
const (
	OOOSenderWindow = int64(4 * 24 * 60 * 60)
	oooMessageKeep  = int64(30 * 24 * 60 * 60)
	MaxOOOSenders   = 20000
)

// ClaimOutOfOfficeReply reports whether the reply to messageKey from sender
// may be sent now, and records it if so. It is false when messageKey was
// claimed before, or sender was answered in this period less than
// OOOSenderWindow ago, or the sender table is full. One transaction, so a
// retry or a second worker never claims the same reply twice.
func (s *Store) ClaimOutOfOfficeReply(ctx context.Context, messageKey, sender, period string, now int64) (bool, error) {
	claimed := false
	err := s.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ooo_messages WHERE at <= ?`, now-oooMessageKeep); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ooo_senders WHERE at <= ?`, now-OOOSenderWindow); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO ooo_messages(message_key, at) VALUES(?, ?) ON CONFLICT(message_key) DO NOTHING`, messageKey, now)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		var known, total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ooo_senders WHERE sender = ?`, sender).Scan(&known); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ooo_senders`).Scan(&total); err != nil {
			return err
		}
		if known == 0 && total >= MaxOOOSenders {
			return nil
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO ooo_senders(sender, period, at) VALUES(?, ?, ?)
			ON CONFLICT(sender) DO UPDATE SET period = excluded.period, at = excluded.at
			WHERE ooo_senders.period <> excluded.period`, sender, period, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("claim out-of-office reply (%s): %w", s.baseDir, err)
	}
	return claimed, nil
}
