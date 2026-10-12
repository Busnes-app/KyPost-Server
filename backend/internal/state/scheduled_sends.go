package state

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// Scheduled send states. scheduled, sending and returning are pending; the rest
// are final and hold no sealed job.
//
//	scheduled --claim--> sending --> sent | queued | uncertain | scheduled (retry) | returning
//	scheduled --cancel--> returning --draft saved--> returned
const (
	ScheduledWaiting   = "scheduled"
	ScheduledSending   = "sending"
	ScheduledReturning = "returning"
)

var ErrScheduledFull = errors.New("too many scheduled messages, or too much scheduled data; send or cancel one first")

// ScheduledSend is one row; Sealed is empty in listings and final rows.
type ScheduledSend struct {
	ID, UserID, Mailbox, Status, Reason string
	SendAt, CreatedAt, ReturningSince   int64
	Sealed, Summary                     []byte
}

func scheduledFinal(status string) bool {
	return status != ScheduledWaiting && status != ScheduledSending && status != ScheduledReturning
}

// AddScheduledSend inserts a waiting row unless the user already has limit
// pending rows or maxBytes of pending sealed data; the checks and the insert
// are one statement.
func (s *Store) AddScheduledSend(ctx context.Context, r ScheduledSend, limit int, maxBytes int64) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO scheduled_sends(id,user_id,mailbox,send_at,status,sealed,summary,created_at)
		SELECT ?1,?2,?3,?4,'scheduled',?5,?6,?7 FROM (SELECT count(*) AS n, coalesce(sum(length(sealed)),0) AS bytes
			FROM scheduled_sends WHERE user_id=?2 AND status IN ('scheduled','sending','returning'))
		WHERE n < ?8 AND bytes + coalesce(length(?5),0) <= ?9`,
		r.ID, r.UserID, r.Mailbox, r.SendAt, r.Sealed, r.Summary, r.CreatedAt, limit, maxBytes)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(ErrScheduledFull, err)
	}
	return nil
}

// ClaimScheduledSend moves one due waiting row to sending. The single UPDATE is
// the exclusivity: across goroutines and processes only one caller gets a row.
// A user with a row already sending is skipped, so one account's slow relay
// holds at most one worker; fresh rows go before retries.
func (s *Store) ClaimScheduledSend(ctx context.Context, now int64) (ScheduledSend, bool, error) {
	r := ScheduledSend{Status: ScheduledSending}
	err := s.db.QueryRowContext(ctx, `UPDATE scheduled_sends SET status='sending'
		WHERE id=(SELECT id FROM scheduled_sends WHERE status='scheduled' AND send_at<=?1 AND next_attempt<=?1
			AND user_id NOT IN (SELECT user_id FROM scheduled_sends WHERE status='sending') ORDER BY next_attempt, send_at LIMIT 1) AND status='scheduled'
		RETURNING id,user_id,mailbox,send_at,reason,sealed,summary,created_at`, now).
		Scan(&r.ID, &r.UserID, &r.Mailbox, &r.SendAt, &r.Reason, &r.Sealed, &r.Summary, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduledSend{}, false, nil
	}
	return r, err == nil, err
}

// MoveScheduledSend changes status only from the expected one, clearing the
// sealed job when the new status is final. nextAttempt delays the next claim
// or draft attempt.
func (s *Store) MoveScheduledSend(ctx context.Context, id, from, to, reason string, nextAttempt int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_sends SET status=?, reason=?, next_attempt=?, sealed=CASE WHEN ? THEN NULL ELSE sealed END,
		returning_since=CASE WHEN ? THEN unixepoch() ELSE returning_since END WHERE id=? AND status=?`,
		to, reason, nextAttempt, scheduledFinal(to), to == ScheduledReturning && from != ScheduledReturning, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InterruptScheduledSends marks rows a stopped process left mid-send uncertain:
// they may have reached the relay, so they are never retried. Run only before
// this process claims anything.
func (s *Store) InterruptScheduledSends(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scheduled_sends SET status='uncertain', reason=?, sealed=NULL WHERE status='sending'`, ScheduledInterrupted)
	return err
}

const ScheduledInterrupted = "the server stopped while sending; check with your recipients before sending again"

// HasScheduledWork reports whether any row is waiting to send or to return.
func (s *Store) HasScheduledWork(ctx context.Context) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM scheduled_sends WHERE status IN ('scheduled','returning'))`).Scan(&pending)
	return pending, err
}

// ScheduledReturns lists returning rows whose draft is due to be saved, the
// earliest per user, so one account's unreachable mailbox cannot fill a sweep.
func (s *Store) ScheduledReturns(ctx context.Context, now int64) ([]ScheduledSend, error) {
	return s.scheduledRows(ctx, `SELECT id,user_id,mailbox,min(send_at),status,reason,sealed,summary,created_at,returning_since
		FROM scheduled_sends WHERE status='returning' AND next_attempt<=? GROUP BY user_id LIMIT 100`, now)
}

// ListScheduledSends returns one mailbox's rows, newest send time first, without the sealed job.
func (s *Store) ListScheduledSends(ctx context.Context, userID, mailbox string) ([]ScheduledSend, error) {
	return s.scheduledRows(ctx, `SELECT id,user_id,mailbox,send_at,status,reason,NULL,summary,created_at,returning_since FROM scheduled_sends WHERE user_id=? AND mailbox=? ORDER BY send_at DESC LIMIT 200`, userID, mailbox)
}

// GetScheduledSend reads one row, sealed job included.
func (s *Store) GetScheduledSend(ctx context.Context, id string) (ScheduledSend, bool, error) {
	rows, err := s.scheduledRows(ctx, `SELECT id,user_id,mailbox,send_at,status,reason,sealed,summary,created_at,returning_since FROM scheduled_sends WHERE id=?`, id)
	if err != nil || len(rows) == 0 {
		return ScheduledSend{}, false, err
	}
	return rows[0], true, nil
}

// PruneScheduledSends drops final rows created before the cutoff.
func (s *Store) PruneScheduledSends(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_sends WHERE created_at<? AND status NOT IN ('scheduled','sending','returning')`, before)
	return err
}

func (s *Store) scheduledRows(ctx context.Context, query string, args ...any) ([]ScheduledSend, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledSend
	for rows.Next() {
		var r ScheduledSend
		if err := rows.Scan(&r.ID, &r.UserID, &r.Mailbox, &r.SendAt, &r.Status, &r.Reason, &r.Sealed, &r.Summary, &r.CreatedAt, &r.ReturningSince); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScheduledRestored is the reason restored waiting mail goes back to Drafts.
const ScheduledRestored = "restored from a backup; review it and send again"

// FenceRestoredScheduledSends runs on stopped restore staging: the original
// server may already have sent what the backup still lists as waiting, so
// waiting rows return to Drafts and interrupted ones become uncertain.
func FenceRestoredScheduledSends(stateDir string) (err error) {
	absolute, err := filepath.Abs(filepath.Join(stateDir, "state.db"))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absolute); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolute}).String()+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='scheduled_sends'").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	if _, err = db.Exec(`UPDATE scheduled_sends SET status='returning', reason=?, next_attempt=0 WHERE status='scheduled'`, ScheduledRestored); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE scheduled_sends SET status='uncertain', reason=?, sealed=NULL WHERE status='sending'`, ScheduledInterrupted)
	return err
}
