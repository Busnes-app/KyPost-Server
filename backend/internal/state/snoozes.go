package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxSnoozes bounds the snoozes table per mailbox.
const MaxSnoozes = 1000

// WokenPinDays is how long a woken message stays pinned to the top of the inbox.
const WokenPinDays = 30

// ErrTooManySnoozes is returned when a mailbox already holds MaxSnoozes rows.
var ErrTooManySnoozes = errors.New("too many snoozed messages")

// Snooze is one snoozes row. WokenAt is 0 while the message is still snoozed.
type Snooze struct {
	MessageID string
	WakeAt    int64
	InboxID   string
	WokenAt   int64
}

// Snooze records messageID as snoozed until wakeAt. Snoozing again replaces the
// time and clears any earlier wake.
func (s *Store) Snooze(messageID string, wakeAt int64) error {
	err := s.tx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM snoozes WHERE message_id <> ?`, messageID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSnoozes {
			return ErrTooManySnoozes
		}
		_, err := tx.Exec(`INSERT INTO snoozes(message_id, wake_at) VALUES(?, ?)
			ON CONFLICT(message_id) DO UPDATE SET wake_at = excluded.wake_at, inbox_id = '', woken_at = 0`,
			messageID, wakeAt)
		return err
	})
	if err != nil {
		return fmt.Errorf("snooze (%s): %w", s.baseDir, err)
	}
	return nil
}

// DueSnoozes lists still-snoozed messages whose wake time is at or before now.
func (s *Store) DueSnoozes(now int64) ([]Snooze, error) {
	return s.snoozes(`WHERE woken_at = 0 AND wake_at <= ? ORDER BY wake_at`, now)
}

// ListSnoozes lists messages that are still snoozed.
func (s *Store) ListSnoozes() ([]Snooze, error) {
	return s.snoozes(`WHERE woken_at = 0 ORDER BY wake_at`)
}

// Woken lists woken messages still pinned to the inbox.
func (s *Store) Woken() ([]Snooze, error) {
	return s.snoozes(`WHERE woken_at > 0 AND inbox_id <> '' ORDER BY woken_at DESC`)
}

// Unpinned lists woken messages whose INBOX id is not known yet.
func (s *Store) Unpinned() ([]Snooze, error) {
	return s.snoozes(`WHERE woken_at > 0 AND inbox_id = ''`)
}

// MarkWoken records that messageID is back in INBOX as inboxID. An empty
// inboxID records the wake without pinning.
func (s *Store) MarkWoken(messageID, inboxID string, at int64) error {
	if _, err := s.db.Exec(`UPDATE snoozes SET inbox_id = ?, woken_at = ? WHERE message_id = ?`, inboxID, at, messageID); err != nil {
		return fmt.Errorf("mark woken (%s): %w", s.baseDir, err)
	}
	return nil
}

// DropSnooze forgets messageID.
func (s *Store) DropSnooze(messageID string) error {
	if _, err := s.db.Exec(`DELETE FROM snoozes WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("drop snooze (%s): %w", s.baseDir, err)
	}
	return nil
}

func (s *Store) snoozes(where string, args ...any) ([]Snooze, error) {
	rows, err := s.db.Query(`SELECT message_id, wake_at, inbox_id, woken_at FROM snoozes `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read snoozes (%s): %w", s.baseDir, err)
	}
	defer rows.Close()
	var out []Snooze
	for rows.Next() {
		var z Snooze
		if err := rows.Scan(&z.MessageID, &z.WakeAt, &z.InboxID, &z.WokenAt); err != nil {
			return nil, fmt.Errorf("read snoozes (%s): %w", s.baseDir, err)
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

func pruneWoken(tx *sql.Tx, now time.Time) error {
	_, err := tx.Exec(`DELETE FROM snoozes WHERE woken_at > 0 AND woken_at < ?`, now.Add(-WokenPinDays*24*time.Hour).Unix())
	return err
}
