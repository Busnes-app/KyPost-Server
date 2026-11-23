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

// Grant is an org's last grant. A missing row is the zero Grant, which fits nothing.
type Grant struct {
	Used, Ceiling int64
	// Admitted is a monotonic watermark of locally reserved growth, never reset by a report.
	Admitted int64
	// Reported is the measured usage in the last accepted report, before reservations.
	Reported int64
	Expires  time.Time
}

// StorageGrant reads org's grant.
func StorageGrant(ctx context.Context, db *sql.DB, org string) (Grant, error) {
	var g Grant
	var expires int64
	err := db.QueryRowContext(ctx, `SELECT used_bytes, ceiling_bytes, expires_at, admitted_bytes, reported_bytes FROM org_storage WHERE org_id = ?`, org).Scan(&g.Used, &g.Ceiling, &expires, &g.Admitted, &g.Reported)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, nil
	}
	g.Expires = time.Unix(expires, 0)
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

// SetGrant records the answer to report seq. g.Admitted is the watermark read BEFORE
// measuring usage; growth reserved since then is retained, including while HTTP waited.
// A scan can count some of that growth twice, conservatively, until the next report.
// A reservation already in flight when that watermark was read can escape the scan:
// at most one bounded write per mailbox writer or photo-directory lock, plus receiving
// copies under its quota fences. This is the documented report-overlap bound.
// Failed writes also stay charged until the next report.
// A newer report drawn since wins: the write
// applies only while seq is still the org's latest.
func SetGrant(ctx context.Context, db *sql.DB, org string, seq int64, g Grant) error {
	_, err := db.ExecContext(ctx, `UPDATE org_storage SET used_bytes = ? + admitted_bytes - ?, reported_bytes = ?, ceiling_bytes = ?, expires_at = ? WHERE org_id = ? AND seq = ?`,
		g.Used, g.Admitted, g.Used, g.Ceiling, g.Expires.Unix(), org, seq)
	return err
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
		case seen, r.Seq < 0, r.CeilingBytes < 0, r.CeilingBytes > maxBytes, r.TTLSeconds < 0:
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
