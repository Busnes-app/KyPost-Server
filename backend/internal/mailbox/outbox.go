package mailbox

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

var ErrOutbound = errors.New("native outbox unavailable or conflicting; preserve queued mail and reconcile ownership, relay and delivery evidence")

// OutboundJob contains immutable, already-authorized wire deliveries and a
// separately prepared Sent copy. Even signed-only wire MIME is encrypted on
// disk. This is server custody, not end-to-end confidentiality.
type OutboundJob struct {
	From               string
	RelayGeneration    string
	Deliveries         []OutboundDelivery
	Sent               []byte
	MaterialGeneration uint64
	DeviceID           string
}
type OutboundDelivery struct {
	Recipients []string
	Raw        []byte
}
type OutboundStatus struct {
	Sequence    int
	State       string
	Attempts    int
	NextAttempt int64
}

const maxOutboundBytes = 128 << 20

func outboundID(id string) bool { return len(id) == 36 && fsutil.SafePathComponent(id) }

func (s *Store) outboundKey(master []byte, id string) ([]byte, error) {
	if len(master) != 32 || !outboundID(id) {
		return nil, ErrOutbound
	}
	// Domain separation reuses the retained relay master key; immutable mailbox
	// namespace and job ID prevent copying ciphertext between owners/jobs.
	binding, err := json.Marshal([]any{"kypost:outbox:v1", s.owner, s.namespace, id})
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, master, nil, string(binding), 32)
}

func (s *Store) validateOutbound(job OutboundJob) error {
	from, err := mail.ParseAddress(job.From)
	if err != nil || from.Address != job.From || strings.ContainsAny(job.From, "\r\n\x00") || !outboundID(job.RelayGeneration) || len(job.Deliveries) == 0 || len(job.Deliveries) > 100 || len(job.DeviceID) > 512 || !strings.Contains(job.From, "@") {
		return ErrOutbound
	}
	total := len(job.Sent)
	if int64(total) > s.limits.MessageBytes {
		return mailmsg.ErrMessageTooLarge
	}
	if len(job.Sent) > 0 {
		if _, _, err := rawMetadata(job.Sent); err != nil {
			return ErrOutbound
		}
	}
	recipients := map[string]bool{}
	for _, delivery := range job.Deliveries {
		if len(delivery.Recipients) == 0 || len(delivery.Recipients) > 100 || int64(len(delivery.Raw)) > s.limits.MessageBytes {
			return ErrOutbound
		}
		normalized, err := mailmsg.NormalizeSMTPMessage(delivery.Raw)
		if err != nil || !bytes.Equal(normalized, delivery.Raw) {
			return ErrOutbound
		}
		msg, err := mail.ReadMessage(bytes.NewReader(delivery.Raw))
		if err != nil {
			return ErrOutbound
		}
		headerFrom, err := mail.ParseAddress(msg.Header.Get("From"))
		h := textproto.MIMEHeader(msg.Header)
		if err != nil || headerFrom.Address != job.From || len(h.Values("From")) != 1 || len(h.Values("Bcc")) != 0 || len(h.Values("Resent-Bcc")) != 0 || len(h.Values("Resent-From")) != 0 || len(h.Values("Resent-Sender")) != 0 {
			return ErrOutbound
		}
		if sender := h.Values("Sender"); len(sender) > 0 {
			a, err := mail.ParseAddress(sender[0])
			if len(sender) != 1 || err != nil || a.Address != job.From {
				return ErrOutbound
			}
		}
		for _, recipient := range delivery.Recipients {
			a, err := mail.ParseAddress(recipient)
			canonical := strings.ToLower(recipient)
			if err != nil || a.Address != recipient || strings.ContainsAny(recipient, "\r\n\x00") || recipients[canonical] || len(recipients) >= 100 {
				return ErrOutbound
			}
			recipients[canonical] = true
		}
		total += len(delivery.Raw)
	}
	if total > 64<<20 {
		return mailmsg.ErrMessageTooLarge
	}
	return nil
}

// QueueOutbound must run inside the caller's current authority fence. It commits
// every delivery and reserves Sent capacity before any network I/O. Exact
// idempotent replay retains all states; differing intent is never adopted.
func (s *Store) QueueOutbound(ctx context.Context, master []byte, id string, job OutboundJob) error {
	if err := s.validateOutbound(job); err != nil {
		return err
	}
	key, err := s.outboundKey(master, id)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(job)
	if err != nil {
		return err
	}
	envelope, err := cryptutil.Seal(plain, key)
	if err != nil {
		return ErrOutbound
	}
	sealed, err := json.Marshal(envelope)
	if err != nil || len(sealed) > maxOutboundBytes {
		return ErrOutbound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE id=?", id).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		prior, _, _, err := s.readOutbound(ctx, tx, master, id)
		if err != nil {
			return err
		}
		old, _ := json.Marshal(prior)
		if !bytes.Equal(old, plain) {
			return ErrConflict
		}
		return tx.Commit()
	}
	var used, count int64
	if err = tx.QueryRowContext(ctx, "SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&count, &used); err != nil {
		return err
	}
	reserved := 0
	if len(job.Sent) > 0 {
		reserved = 1
	}
	if count+int64(1+len(job.Deliveries)+reserved) > int64(s.limits.Records) || int64(len(sealed)+len(job.Sent)) > s.limits.PayloadBytes-used {
		return ErrCapacity
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES(?,?,?,?)", id, sealed, len(job.Sent), reserved); err != nil {
		return err
	}
	for sequence := range job.Deliveries {
		if _, err = tx.ExecContext(ctx, "INSERT INTO outbox_deliveries(job,sequence,state) VALUES(?,?,'queued')", id, sequence); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) readOutbound(ctx context.Context, tx *sql.Tx, master []byte, id string) (OutboundJob, []OutboundStatus, int64, error) {
	key, err := s.outboundKey(master, id)
	if err != nil {
		return OutboundJob{}, nil, 0, err
	}
	var raw []byte
	var size, sentBytes, sentReserved int64
	var sentID sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT CASE WHEN length(ciphertext)<=? THEN ciphertext ELSE NULL END,length(ciphertext),sent_bytes,sent_reserved,sent_id FROM outbox WHERE id=?", maxOutboundBytes, id).Scan(&raw, &size, &sentBytes, &sentReserved, &sentID)
	if err != nil {
		return OutboundJob{}, nil, 0, ErrOutbound
	}
	envelope, ok := cryptutil.ParseEnvelope(raw)
	if !ok || size > maxOutboundBytes {
		return OutboundJob{}, nil, 0, ErrOutbound
	}
	plain, err := cryptutil.Open(envelope, key)
	var job OutboundJob
	if err != nil || json.Unmarshal(plain, &job) != nil || s.validateOutbound(job) != nil {
		return OutboundJob{}, nil, 0, ErrOutbound
	}
	if sentID.Valid {
		if sentBytes != 0 || sentReserved != 0 || len(job.Sent) == 0 {
			return OutboundJob{}, nil, 0, ErrOutbound
		}
		var hash string
		if tx.QueryRowContext(ctx, "SELECT digest FROM messages WHERE id=?", sentID.Int64).Scan(&hash) != nil || hash != hashIncoming(job.Sent) {
			return OutboundJob{}, nil, 0, ErrOutbound
		}
	} else if sentBytes != int64(len(job.Sent)) || (len(job.Sent) > 0 && sentReserved != 1) || (len(job.Sent) == 0 && sentReserved != 0) {
		return OutboundJob{}, nil, 0, ErrOutbound
	}
	rows, err := tx.QueryContext(ctx, "SELECT sequence,state,attempts,next_attempt,claim FROM outbox_deliveries WHERE job=? ORDER BY sequence", id)
	if err != nil {
		return OutboundJob{}, nil, 0, err
	}
	defer rows.Close()
	statuses := []OutboundStatus{}
	for rows.Next() {
		var status OutboundStatus
		var claim string
		if err = rows.Scan(&status.Sequence, &status.State, &status.Attempts, &status.NextAttempt, &claim); err != nil {
			return OutboundJob{}, nil, 0, err
		}
		if status.Sequence != len(statuses) || status.Attempts < 0 || status.Attempts > 6 || status.NextAttempt < 0 || (status.State == "queued" && (status.Attempts != 0 || claim != "")) || (status.State != "queued" && status.State != "quarantined" && (!outboundID(claim) || status.Attempts == 0)) {
			return OutboundJob{}, nil, 0, ErrOutbound
		}
		switch status.State {
		case "queued", "submitting", "accepted", "retryable", "failed", "uncertain", "quarantined":
		default:
			return OutboundJob{}, nil, 0, ErrOutbound
		}
		statuses = append(statuses, status)
	}
	if rows.Err() != nil || len(statuses) != len(job.Deliveries) {
		return OutboundJob{}, nil, 0, ErrOutbound
	}
	return job, statuses, sentID.Int64, nil
}

func (s *Store) ReadOutbound(ctx context.Context, master []byte, id string) (OutboundJob, []OutboundStatus, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboundJob{}, nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	job, statuses, sent, err := s.readOutbound(ctx, tx, master, id)
	if err != nil {
		return OutboundJob{}, nil, 0, err
	}
	return job, statuses, sent, tx.Commit()
}

// ClaimOutbound runs under current domain/directory/users/key/device/alias
// authority. Its durable claim is the authorization boundary; release all
// authority locks before network I/O. A submitting/crashed job is never reclaimed
// on lease expiry. Historical backup evidence is not authority to call this.
func (s *Store) ClaimOutbound(ctx context.Context, master []byte, id string, sequence int, generation string) (OutboundDelivery, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboundDelivery{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	job, statuses, _, err := s.readOutbound(ctx, tx, master, id)
	if err != nil || sequence < 0 || sequence >= len(statuses) || job.RelayGeneration != generation {
		return OutboundDelivery{}, "", ErrOutbound
	}
	status := statuses[sequence]
	if (status.State != "queued" && status.State != "retryable") || status.NextAttempt > time.Now().Unix() || status.Attempts >= 6 {
		return OutboundDelivery{}, "", ErrOutbound
	}
	claim, err := fsutil.NewUUIDv4()
	if err != nil {
		return OutboundDelivery{}, "", err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE outbox_deliveries SET state='submitting',claim=?,attempts=attempts+1,next_attempt=0 WHERE job=? AND sequence=?", claim, id, sequence); err != nil {
		return OutboundDelivery{}, "", err
	}
	return job.Deliveries[sequence], claim, tx.Commit()
}

// CompleteOutbound fences late/duplicate completions by the immutable attempt
// token. Generic failures are conservatively uncertain. Only definite 4xx can
// retry automatically, with six total attempts and bounded backoff.
func (s *Store) CompleteOutbound(ctx context.Context, id string, sequence int, claim string, submission error) error {
	if !outboundID(id) || !outboundID(claim) {
		return ErrOutbound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state, token string
	var attempts int
	if err = tx.QueryRowContext(ctx, "SELECT state,claim,attempts FROM outbox_deliveries WHERE job=? AND sequence=?", id, sequence).Scan(&state, &token, &attempts); err != nil || state != "submitting" || token != claim || attempts < 1 || attempts > 6 {
		return ErrOutbound
	}
	outcome := "uncertain"
	var retryAt int64
	if submission == nil || errors.Is(submission, mailmsg.ErrSMTPAcceptedThenFailed) {
		outcome = "accepted"
	} else if !errors.Is(submission, mailmsg.ErrSMTPAcceptanceUncertain) {
		var response *textproto.Error
		if errors.As(submission, &response) && response.Code >= 400 && response.Code <= 599 {
			outcome = "failed"
			if response.Code < 500 && attempts < 6 {
				outcome = "retryable"
				retryAt = time.Now().Add(time.Duration(30*(1<<(attempts-1))) * time.Second).Unix()
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE outbox_deliveries SET state=?,next_attempt=? WHERE job=? AND sequence=?", outcome, retryAt, id, sequence); err != nil {
		return err
	}
	return tx.Commit()
}

// FileOutboundSent commits the reserved exact copy and receipt together. Retry
// returns the original ID even after move/deletion and never submits SMTP.
func (s *Store) FileOutboundSent(ctx context.Context, master []byte, id string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	job, statuses, sent, err := s.readOutbound(ctx, tx, master, id)
	if err != nil {
		return 0, err
	}
	if sent > 0 {
		return sent, tx.Commit()
	}
	accepted := false
	for _, status := range statuses {
		accepted = accepted || status.State == "accepted"
	}
	if !accepted || len(job.Sent) == 0 {
		return 0, ErrOutbound
	}
	headers, at, err := rawMetadata(job.Sent)
	if err != nil {
		return 0, ErrOutbound
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(folder,raw,digest,sender,subject,sent_to,cc,bcc,at_utc,seen,draft) VALUES('Sent',?,?,?,?,?,?,?,?,1,0)`, job.Sent, hashIncoming(job.Sent), headers.Get("From"), headers.Get("Subject"), headers.Get("To"), headers.Get("Cc"), headers.Get("Bcc"), at.Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	sent, err = result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE outbox SET sent_id=?,sent_bytes=0,sent_reserved=0 WHERE id=?", sent, id); err != nil {
		return 0, err
	}
	if err = change(ctx, tx, sent, "Sent", false); err != nil {
		return 0, err
	}
	return sent, tx.Commit()
}

// ValidateOutboundSnapshot reads a frozen backup database without migrating it.
// It checks historical ciphertext/owner/receipt/quota consistency only. It never
// claims a job or converts restored evidence into current sending permission.
func ValidateOutboundSnapshot(ctx context.Context, path string, master []byte, relay mailmsg.DomainRelay) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	location := url.URL{Scheme: "file", Path: absolute}
	db, err := sql.Open("sqlite", location.String()+"?mode=ro")
	if err != nil {
		return false, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var exists int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('outbox','outbox_deliveries')").Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, nil
	} // Older native databases had no queue.
	if exists != 2 {
		return true, ErrOutbound
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM outbox").Scan(&count); err != nil {
		return false, err
	}
	var orphaned int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check WHERE "table" IN ('outbox','outbox_deliveries')`).Scan(&orphaned); err != nil || orphaned != 0 {
		return true, ErrOutbound
	}
	var deliveries int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM outbox_deliveries").Scan(&deliveries); err != nil {
		return true, ErrOutbound
	}
	if count == 0 {
		if deliveries != 0 {
			return true, ErrOutbound
		}
		return false, nil
	}
	s := &Store{db: db}
	if relay.Validate() != nil || len(master) != 32 {
		return true, ErrOutbound
	}
	if err = db.QueryRowContext(ctx, "SELECT issuer,subject,mailbox,message_bytes,payload_bytes,records FROM identity WHERE id=1").Scan(&s.owner.Issuer, &s.owner.Subject, &s.owner.Mailbox, &s.limits.MessageBytes, &s.limits.PayloadBytes, &s.limits.Records); err != nil {
		return true, ErrOutbound
	}
	if s.owner.Issuer != relay.Issuer || count > s.limits.Records || db.QueryRowContext(ctx, "SELECT token FROM namespace WHERE id=1").Scan(&s.namespace) != nil || s.namespace == "" {
		return true, ErrOutbound
	}
	var records, used, wantRecords, wantUsed int64
	if err = db.QueryRowContext(ctx, `SELECT
 (SELECT records FROM usage WHERE id=1),(SELECT payload_bytes FROM usage WHERE id=1),
 (SELECT count(*) FROM messages)+(SELECT count(*)+coalesce(sum(sent_reserved),0) FROM outbox)+(SELECT count(*) FROM outbox_deliveries),
 (SELECT coalesce(sum(length(raw)),0) FROM messages)+(SELECT coalesce(sum(length(ciphertext)+sent_bytes),0) FROM outbox)`).Scan(&records, &used, &wantRecords, &wantUsed); err != nil || records != wantRecords || used != wantUsed || records > int64(s.limits.Records) || used > s.limits.PayloadBytes {
		return true, ErrOutbound
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM outbox ORDER BY id")
	if err != nil {
		return true, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return true, err
		}
		if !outboundID(id) || len(ids) >= s.limits.Records {
			_ = rows.Close()
			return true, ErrOutbound
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return true, err
	}
	for _, id := range ids {
		job, statuses, sent, err := s.ReadOutbound(ctx, master, id)
		if err != nil {
			return true, err
		}
		_, domain, _ := strings.Cut(job.From, "@")
		if strings.ToLower(domain) != relay.Domain {
			return true, ErrOutbound
		}
		accepted := false
		for _, status := range statuses {
			accepted = accepted || status.State == "accepted"
		}
		if sent != 0 && !accepted {
			return true, ErrOutbound
		}
	}
	return true, nil
}

// QuarantineOutbound retains unclaimed intent after authority/configuration
// changes. It never recalls an already-authorized in-flight submission.
func (s *Store) QuarantineOutbound(ctx context.Context, id string, sequence int) error {
	if !outboundID(id) || sequence < 0 {
		return ErrOutbound
	}
	result, err := s.db.ExecContext(ctx, "UPDATE outbox_deliveries SET state='quarantined',next_attempt=0 WHERE job=? AND sequence=? AND state IN ('queued','retryable','quarantined')", id, sequence)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrOutbound
	}
	return nil
}

// PendingOutbound finds bounded local work in oldest-first order. Crashed or
// uncertain submissions are deliberately excluded; Sent retry is independent.
// Discovery is not authorization: callers must revalidate before claiming.
func (s *Store) PendingOutbound(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrOutbound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT o.id,o.created_at FROM outbox o JOIN outbox_deliveries d ON d.job=o.id
 WHERE (d.state IN ('queued','retryable') AND d.next_attempt<=?) OR (d.state='accepted' AND o.sent_reserved=1)
 ORDER BY o.created_at,o.id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		var at int64
		if err = rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if !outboundID(id) || at <= 0 {
			return nil, ErrOutbound
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
