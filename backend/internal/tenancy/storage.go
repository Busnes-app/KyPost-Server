package tenancy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
)

// The org storage pool (K "Quotas"; KyIdentity tenancy spec "Org storage pool", S4).
// KyPost reports its usage per org to POST /api/suite/storage and gets a headroom grant: a
// ceiling on its own usage, kept in org_storage so every replica checks one figure. A
// write that adds bytes fits only while the grant is unexpired and usage plus the write
// stays within the ceiling.

// GrantTTL bounds a grant's life whatever ttl_seconds says (KyIdentity answers 900).
const GrantTTL = 15 * time.Minute

// MaxStorageReports is the most reports one request carries (KyIdentity's bound).
const MaxStorageReports = 500

// maxStorageAnswer bounds the answer: 500 results of about 200 bytes each.
const maxStorageAnswer = 1 << 20

// maxBytes is the largest byte figure either side may send (2^53).
const maxBytes = 1 << 53

// Grant is an org's last grant, with the pool KyIdentity reported beside it. A missing row
// is the zero Grant, which fits nothing and has no pool. Zero times are unset.
type Grant struct {
	Used, Ceiling      int64
	Admitted, Reported int64
	Expires            time.Time
	// PoolBytes is the org's pool; OrgUsed is every app's usage, KyPost's last report
	// included; OverSince is KyIdentity's start of the overrun.
	PoolBytes, OrgUsed int64
	OverSince          time.Time
	// FirstOver is KyPost's own first sighting of the overrun, GraceEnd the moment the
	// receiving overage ran out (PoolMail); both are written once per overrun.
	FirstOver, GraceEnd time.Time
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// StorageGrant reads org's grant.
func StorageGrant(ctx context.Context, db *sql.DB, org string) (Grant, error) {
	var g Grant
	var expires, over, first, end int64
	err := db.QueryRowContext(ctx, `SELECT used_bytes, ceiling_bytes, expires_at, admitted_bytes, reported_bytes, pool_bytes, org_used_bytes, over_since, first_over, grace_end FROM org_storage WHERE org_id = ?`, org).
		Scan(&g.Used, &g.Ceiling, &expires, &g.Admitted, &g.Reported, &g.PoolBytes, &g.OrgUsed, &over, &first, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, nil
	}
	g.Expires, g.OverSince, g.FirstOver, g.GraceEnd = time.Unix(expires, 0), timeOrZero(over), timeOrZero(first), timeOrZero(end)
	return g, err
}

// NextStorageSeq draws org's next report sequence number from the shared database, so
// replicas never reuse one. It returns the org's last grant too, for sizing the request.
func NextStorageSeq(ctx context.Context, db *sql.DB, org string) (int64, Grant, error) {
	var seq, expires int64
	var g Grant
	err := db.QueryRowContext(ctx, `INSERT INTO org_storage(org_id, seq, used_bytes, ceiling_bytes, expires_at) VALUES(?, 1, 0, 0, 0)
ON CONFLICT(org_id) DO UPDATE SET seq = org_storage.seq + 1 RETURNING seq, used_bytes, ceiling_bytes, expires_at, admitted_bytes, reported_bytes`, org).Scan(&seq, &g.Used, &g.Ceiling, &expires, &g.Admitted, &g.Reported)
	g.Expires = time.Unix(expires, 0)
	return seq, g, err
}

// SetGrant preserves reservations since g.Admitted (the pre-scan watermark). See the
// report-overlap bound in the tenancy design; failed writes stay charged until a report.
// SetGrant records the answer to report seq: the grant and the pool. A newer report
// drawn since wins: the write applies only while seq is still the org's latest. An
// answer with the org back within its pool ends the overrun, so the next one gets a
// fresh grace window.
func SetGrant(ctx context.Context, db *sql.DB, org string, seq int64, g Grant) error {
	within := 0
	if g.OverSince.IsZero() && g.OrgUsed <= g.PoolBytes {
		within = 1
	}
	_, err := db.ExecContext(ctx, `UPDATE org_storage SET used_bytes = ? + admitted_bytes - ?, reported_bytes = ?, ceiling_bytes = ?, expires_at = ?, pool_bytes = ?, org_used_bytes = ?, over_since = ?,
 first_over = CASE WHEN ? = 1 THEN 0 ELSE first_over END, grace_end = CASE WHEN ? = 1 THEN 0 ELSE grace_end END
WHERE org_id = ? AND seq = ?`,
		g.Used, g.Admitted, g.Used, g.Ceiling, g.Expires.Unix(), g.PoolBytes, g.OrgUsed, unixOrZero(g.OverSince), within, within, org, seq)
	return err
}

// MarkOverrun writes KyPost's first sighting of org's overrun and the end of its
// receiving overage, each only if unset (zero arguments write nothing), so every
// replica keeps the first value.
func MarkOverrun(ctx context.Context, db *sql.DB, org string, firstOver, graceEnd time.Time) error {
	if firstOver.IsZero() && graceEnd.IsZero() {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE org_storage SET first_over = CASE WHEN first_over = 0 THEN ? ELSE first_over END,
 grace_end = CASE WHEN grace_end = 0 THEN ? ELSE grace_end END WHERE org_id = ?`, unixOrZero(firstOver), unixOrZero(graceEnd), org)
	return err
}

// Incoming mail over the pool (K open question 16, owner decision of 2026-11-23): 7 days'
// grace with up to PoolOverage(pool) more, whichever ends first; then a temporary refusal
// for 3 days; then a permanent one.
const (
	GracePeriod    = 7 * 24 * time.Hour
	TemporaryAfter = 3 * 24 * time.Hour
	minOverage     = 2_000_000_000 // 2 GB, the overage of the smallest plan's 20 GB pool
)

// PoolOverage is how far past its pool an org's incoming mail may take it during grace:
// 10 % of the pool, and at least 2 GB.
func PoolOverage(pool int64) int64 { return max(pool/10, minOverage) }

// PoolMail is what incoming mail gets from the pool.
type PoolMail int

const (
	PoolMailAccept    PoolMail = iota
	PoolMailTemporary          // 452 4.2.2, exit 9
	PoolMailRefuse             // 552 5.2.2, exit 14
)

// Mail answers a delivery of add bytes when KyPost stores used bytes for the org now. It
// fits the grant like any write, or else falls under the overrun rule, which starts at
// the earlier of KyIdentity's OverSince and KyPost's FirstOver. Grace ends at 7 days or
// once stored usage reaches the overage. firstOver and graceEnd
// are the dates the caller must record (MarkOverrun), zero when there is nothing new. An
// org with no pool (KyIdentity has not reported one) gets the temporary answer and no
// grace (K open question 15).
func (g Grant) Mail(used, add int64, now time.Time) (answer PoolMail, firstOver, graceEnd time.Time) {
	if now.Before(g.Expires) && used >= 0 && add >= 0 && used <= g.Ceiling && add <= g.Ceiling-used {
		return PoolMailAccept, time.Time{}, time.Time{}
	}
	if g.PoolBytes <= 0 {
		return PoolMailTemporary, time.Time{}, time.Time{}
	}
	start := g.FirstOver
	if !g.OverSince.IsZero() && (start.IsZero() || g.OverSince.Before(start)) {
		start = g.OverSince
	}
	if start.IsZero() || start.After(now) {
		start, firstOver = now, now
	}
	end := start.Add(GracePeriod)
	if !g.GraceEnd.IsZero() && g.GraceEnd.Before(end) {
		end = g.GraceEnd
	}
	if now.Before(end) {
		// Every other app's last reported usage, plus KyPost's now.
		total := max(g.OrgUsed-g.Reported, 0) + used
		ceiling := g.PoolBytes + PoolOverage(g.PoolBytes)
		if add >= 0 && total+add <= ceiling {
			return PoolMailAccept, firstOver, time.Time{}
		}
		// Reservations can include failed/in-flight writes. They bound admission but
		// cannot end grace: only the periodic report confirms stored usage. A refused
		// oversized delivery or a failed holding write therefore cannot shorten grace.
		if total < ceiling || g.OrgUsed < ceiling {
			return PoolMailTemporary, firstOver, time.Time{}
		}
		end, graceEnd = now, now
	}
	if now.Before(end.Add(TemporaryAfter)) {
		return PoolMailTemporary, firstOver, graceEnd
	}
	return PoolMailRefuse, firstOver, graceEnd
}

// ReserveStorage atomically spends org's cached headroom across mailboxes, processes
// and replicas. The caller invokes it INSIDE its write transaction, after replay checks.
// Reservations commit first: a failed mailbox/photo write can only overcount until a
// report, never let another writer spend the same bytes. No storage is scanned here.
func ReserveStorage(ctx context.Context, db *sql.DB, org string, add int64, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE org_storage SET used_bytes = used_bytes + ?, admitted_bytes = admitted_bytes + ?
 WHERE org_id = ? AND expires_at > ? AND used_bytes >= 0 AND used_bytes <= ceiling_bytes
 AND ? <= ceiling_bytes - used_bytes AND admitted_bytes <= 9223372036854775807 - ?`,
		add, add, org, now.Unix(), add, add)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MailReservation is all the copies a DATA delivery adds to one org. Limit is the
// optional KyPost storage safety ceiling; the pool and grace still apply separately.
type MailReservation struct {
	Org   string
	Bytes int64
	Limit *int64
}

// ReserveMail admits every org's copies or none, under the shared tenancy writer
// transaction. tenancy.Open uses SQLite immediate transactions or kypg, whose BeginTx
// takes the schema writer advisory lock BEFORE any read (pgdialect/driver.go). Thus
// separate PostgreSQL connections/replicas serialize admission, not just the UPDATE.
// RCPT probes never call it. Refusals persist overrun dates without
// spending bytes; failed holding writes remain charged until the next usage report.
func ReserveMail(ctx context.Context, db *sql.DB, reservations []MailReservation, now time.Time) (PoolMail, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PoolMailTemporary, err
	}
	defer func() { _ = tx.Rollback() }()
	answer := PoolMailAccept
	for _, r := range reservations {
		var g Grant
		var expires, over, first, end int64
		err := tx.QueryRowContext(ctx, `SELECT used_bytes, ceiling_bytes, expires_at, reported_bytes, pool_bytes, org_used_bytes, over_since, first_over, grace_end FROM org_storage WHERE org_id = ?`, r.Org).
			Scan(&g.Used, &g.Ceiling, &expires, &g.Reported, &g.PoolBytes, &g.OrgUsed, &over, &first, &end)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PoolMailTemporary, err
		}
		g.Expires, g.OverSince, g.FirstOver, g.GraceEnd = timeOrZero(expires), timeOrZero(over), timeOrZero(first), timeOrZero(end)
		if r.Limit != nil && (g.Used > *r.Limit || r.Bytes > *r.Limit-g.Used) {
			return PoolMailTemporary, nil
		}
		decision, firstAt, endAt := g.Mail(g.Used, r.Bytes, now)
		answer = max(answer, decision)
		if _, err := tx.ExecContext(ctx, `UPDATE org_storage SET first_over = CASE WHEN first_over = 0 THEN ? ELSE first_over END,
   grace_end = CASE WHEN grace_end = 0 THEN ? ELSE grace_end END WHERE org_id = ?`, unixOrZero(firstAt), unixOrZero(endAt), r.Org); err != nil {
			return PoolMailTemporary, err
		}
	}
	if answer == PoolMailAccept {
		for _, r := range reservations {
			result, err := tx.ExecContext(ctx, `UPDATE org_storage SET used_bytes = used_bytes + ?, admitted_bytes = admitted_bytes + ?
    WHERE org_id = ? AND admitted_bytes <= 9223372036854775807 - ?`, r.Bytes, r.Bytes, r.Org, r.Bytes)
			if err != nil {
				return PoolMailTemporary, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return PoolMailTemporary, err
			}
			if n != 1 {
				return PoolMailTemporary, nil
			}
		}
	}
	return answer, tx.Commit()
}

// ReportableOrgs lists the cached orgs KyPost reports for: every one not deleted.
func ReportableOrgs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM orgs WHERE status <> 'deleted' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// StorageReport is one org's entry in a report.
type StorageReport struct {
	OrgID     string `json:"org_id"`
	UsedBytes int64  `json:"used_bytes"`
	Seq       int64  `json:"seq"`
	WantBytes int64  `json:"want_bytes"`
}

// StorageResult is KyIdentity's answer for one org.
type StorageResult struct {
	OrgID        string `json:"org_id"`
	Result       string `json:"result"`
	Seq          int64  `json:"seq"`
	CeilingBytes int64  `json:"ceiling_bytes"`
	TTLSeconds   int64  `json:"ttl_seconds"`
	PoolBytes    int64  `json:"pool_bytes"`
	OrgUsedBytes int64  `json:"org_used_bytes"`
	OverSince    *int64 `json:"over_since"`
}

// ErrStorageAnswer is a malformed answer; nothing of it is applied.
var ErrStorageAnswer = errors.New("tenancy: malformed storage answer")

// ReportStorage sends reports (1 to MaxStorageReports) with dir's client credentials and
// returns one checked result per report, in the reports' order.
func ReportStorage(ctx context.Context, dir suitedir.Client, reports []StorageReport) ([]StorageResult, error) {
	if len(reports) == 0 || len(reports) > MaxStorageReports {
		return nil, errors.New("tenancy: 1 to 500 storage reports per request")
	}
	body, err := json.Marshal(map[string]any{"reports": reports})
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(dir.Issuer, "/"))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.Scheme != "https" && (base.Scheme != "http" || !loopbackHost(base.Hostname())) {
		return nil, suitedir.ErrInsecureIssuer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("api", "suite", "storage").String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(dir.ClientID, dir.ClientSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := http.Client{Timeout: 10 * time.Second}
	if dir.HTTP != nil {
		client = *dir.HTTP
	}
	// Go would resend the Basic credentials wherever a redirect pointed.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirects are refused") }
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("tenancy: storage report: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxStorageAnswer+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("tenancy: storage report: %w", err)
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, suitedir.ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("tenancy: storage report: status %d", resp.StatusCode)
	case len(raw) > maxStorageAnswer:
		return nil, ErrStorageAnswer
	}
	var answer struct {
		Results []StorageResult `json:"results"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return nil, ErrStorageAnswer
	}
	return matchResults(reports, answer.Results)
}

// matchResults pairs each report with exactly one result for its org, refusing the whole
// answer if any result is unknown, repeated, missing or out of range.
func matchResults(reports []StorageReport, results []StorageResult) ([]StorageResult, error) {
	if len(results) != len(reports) {
		return nil, ErrStorageAnswer
	}
	byOrg := make(map[string]StorageResult, len(results))
	for _, r := range results {
		_, seen := byOrg[r.OrgID]
		switch {
		case seen, r.Seq < 0, r.CeilingBytes < 0, r.CeilingBytes > maxBytes, r.TTLSeconds < 0,
			r.PoolBytes < 0, r.PoolBytes > maxBytes, r.OrgUsedBytes < 0, r.OrgUsedBytes > maxBytes, r.OverSince != nil && *r.OverSince <= 0:
			return nil, ErrStorageAnswer
		case r.Result == "ok", r.Result == "stale", r.Result == "unknown_org":
		default:
			return nil, ErrStorageAnswer
		}
		byOrg[r.OrgID] = r
	}
	out := make([]StorageResult, len(reports))
	for i, rep := range reports {
		r, ok := byOrg[rep.OrgID]
		// A grant answers this report only: its seq must be the one sent.
		if !ok || r.Result == "ok" && r.Seq != rep.Seq {
			return nil, ErrStorageAnswer
		}
		out[i] = r
	}
	return out, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
