package state

import (
	"context"
	"errors"
	"time"
)

const (
	sendHour = int64(time.Hour / time.Second)
	sendDay  = int64(24 * time.Hour / time.Second)
)

var (
	ErrSendQuota    = errors.New("sending quota reached")
	ErrSendTooLarge = errors.New("this message has more recipients than the daily sending quota allows")
)

// AdmitSend charges one message with recipients to userID unless that takes
// the user past messagesPerHour messages in the last hour or recipientsPerDay
// recipients in the last day; the check and the insert are one statement, so
// concurrent senders in any process cannot both squeeze in. A send already
// charged under id is admitted again without counting twice. A refusal says
// how long until the send would fit.
func (s *Store) AdmitSend(ctx context.Context, id, userID string, recipients, messagesPerHour, recipientsPerDay int, now int64) (time.Duration, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM send_ledger WHERE user_id=? AND at<=?`, userID, now-sendDay); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO send_ledger(id,user_id,at,recipients)
		SELECT ?1,?2,?3,?4 FROM (SELECT coalesce(sum(at>?3-?7),0) AS n, coalesce(sum(recipients),0) AS r
			FROM send_ledger WHERE user_id=?2 AND at>?3-?8)
		WHERE n < ?5 AND r + ?4 <= ?6 AND NOT EXISTS (SELECT 1 FROM send_ledger WHERE id=?1)`,
		id, userID, now, recipients, messagesPerHour, recipientsPerDay, sendHour, sendDay)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return 0, err
	}
	var charged bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM send_ledger WHERE id=?)`, id).Scan(&charged); err != nil || charged {
		return 0, err
	}
	if recipients > recipientsPerDay {
		return 0, ErrSendTooLarge
	}
	wait, err := s.sendQuotaWait(ctx, userID, recipients, messagesPerHour, recipientsPerDay, now)
	if err != nil {
		return 0, err
	}
	return wait, ErrSendQuota
}

// sendQuotaWait finds when enough earlier sends age out of both windows.
func (s *Store) sendQuotaWait(ctx context.Context, userID string, recipients, messagesPerHour, recipientsPerDay int, now int64) (time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, recipients FROM send_ledger WHERE user_id=? AND at>? ORDER BY at`, userID, now-sendDay)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type sent struct {
		at         int64
		recipients int
	}
	var day []sent
	var hour []int64
	total, until := 0, now
	for rows.Next() {
		var r sent
		if err := rows.Scan(&r.at, &r.recipients); err != nil {
			return 0, err
		}
		day = append(day, r)
		total += r.recipients
		if r.at > now-sendHour {
			hour = append(hour, r.at)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if over := len(hour) - messagesPerHour; over >= 0 {
		until = max(until, hour[over]+sendHour)
	}
	excess := total + recipients - recipientsPerDay
	for _, r := range day {
		if excess <= 0 {
			break
		}
		excess -= r.recipients
		until = max(until, r.at+sendDay)
	}
	return time.Duration(max(until-now, 1)) * time.Second, nil
}
