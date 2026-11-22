package state

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
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
	res, err := s.db.ExecContext(ctx, dbbackend.SQL(`INSERT INTO send_ledger(id,user_id,at,recipients)
		SELECT ?1,?2,?3,?4 FROM (SELECT coalesce(sum(at>?3-?7),0) AS n, coalesce(sum(recipients),0) AS r
			FROM send_ledger WHERE user_id=?2 AND at>?3-?8)
		WHERE n < ?5 AND r + ?4 <= ?6 AND NOT EXISTS (SELECT 1 FROM send_ledger WHERE id=?1)`,
		`INSERT INTO send_ledger(id,user_id,at,recipients)
		SELECT ?1::text,?2::text,?3::bigint,?4::bigint FROM (SELECT coalesce(sum((at>?3::bigint-?7::bigint)::int),0) AS n, coalesce(sum(recipients),0) AS r
			FROM send_ledger WHERE user_id=?2 AND at>?3::bigint-?8::bigint) q
		WHERE n < ?5 AND r + ?4::bigint <= ?6 AND NOT EXISTS (SELECT 1 FROM send_ledger WHERE id=?1)`),
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

// ClaimSend records id under userID once and reports whether this call made
// the claim, so one delivery happens once across processes and restarts.
// Claims count toward no quota and last a day; ReleaseSend drops one whose
// send definitely did not happen.
func (s *Store) ClaimSend(ctx context.Context, id, userID string, now int64) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM send_ledger WHERE user_id=? AND at<=?`, userID, now-sendDay); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO send_ledger(id,user_id,at,recipients) VALUES(?,?,?,0) ON CONFLICT(id) DO NOTHING`, id, userID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseSend forgets a claim, so a retry of its send is processed again.
func (s *Store) ReleaseSend(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM send_ledger WHERE id=?`, id)
	return err
}

// OrgSendUsage sums the charges of an org's members, as AdmitSend counts one user's:
// messages in the last hour and recipients in the last day (tenancy design, "Abuse
// limits per org"). It reads the shared ledger, so it holds across replicas.
func (s *Store) OrgSendUsage(ctx context.Context, members []string, now int64) (messages, recipients int, err error) {
	if len(members) == 0 {
		return 0, 0, nil
	}
	args := []any{now - sendHour, now - sendDay}
	in := "?" + strings.Repeat(",?", len(members)-1)
	for _, m := range members {
		args = append(args, m)
	}
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN at > ? THEN 1 ELSE 0 END),0), coalesce(sum(recipients),0)
		FROM send_ledger WHERE at > ? AND user_id IN (`+in+`)`, args...).Scan(&messages, &recipients)
	return messages, recipients, err
}

// SendCharged reports whether id is already in the ledger.
func (s *Store) SendCharged(ctx context.Context, id string) (bool, error) {
	var charged bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM send_ledger WHERE id=?)`, id).Scan(&charged)
	return charged, err
}
