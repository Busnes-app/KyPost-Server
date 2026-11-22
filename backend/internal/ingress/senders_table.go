package ingress

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// TableSenders is the sender block list and evidence in tenancy.db's sender_blocks and
// sender_evidence tables (multi mode, receiving tier R7; schema in tenancy/tenancy.go). It
// applies the file rules row by row and never touches the receiving directory. Every
// write is one tenancy.db write transaction, which the store serializes (SQLite's single
// writer, or the PostgreSQL schema's writer lock), as the files' locks do.
func TableSenders(db *sql.DB) SenderStore { return tableSenders{db} }

type tableSenders struct{ db *sql.DB }

const blockColumns = `id, kind, value, until, source, level, created_at, actor, reason`

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readBlocks reads rows, refusing a row the file rule would refuse.
func readBlocks(ctx context.Context, q querier, where string, args ...any) ([]SenderBlock, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+blockColumns+` FROM sender_blocks`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SenderBlock
	for rows.Next() {
		var b SenderBlock
		var until sql.NullInt64
		if err := rows.Scan(&b.ID, &b.Kind, &b.Value, &until, &b.Source, &b.Level, &b.CreatedAt, &b.Actor, &b.Reason); err != nil {
			return nil, err
		}
		if until.Valid {
			b.Until = &until.Int64
		}
		if !b.valid() {
			return nil, errBlocksMalformed
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// allBlocks reads every row under the file's whole-list rule.
func allBlocks(ctx context.Context, q querier) ([]SenderBlock, error) {
	blocks, err := readBlocks(ctx, q, "")
	if err == nil {
		err = checkBlocks(blocks)
	}
	return blocks, err
}

func (t tableSenders) List(ctx context.Context, now time.Time) ([]SenderBlock, error) {
	all, err := allBlocks(ctx, t.db)
	if err != nil {
		return nil, err
	}
	return inForce(all, now), nil
}

// Blocked reads only the sender's address row and its domain's, by primary key.
func (t tableSenders) Blocked(ctx context.Context, sender string, now time.Time) (bool, error) {
	address, domain, ok := blockTargets(sender)
	if !ok {
		return false, nil
	}
	rows, err := readBlocks(ctx, t.db, ` WHERE id IN (?, ?)`, BlockID("address", address), BlockID("domain", domain))
	if err != nil {
		return false, err
	}
	return matches(inForce(rows, now), sender), nil
}

func (t tableSenders) Put(ctx context.Context, b SenderBlock, own []string, now time.Time) (SenderBlock, error) {
	return put(ctx, t.change, b, own, now)
}

func (t tableSenders) Remove(ctx context.Context, id string, now time.Time) (bool, error) {
	if !validID(id) {
		return false, ErrBlockInvalid
	}
	var suppressed error
	if list, err := t.List(ctx, now); err == nil && slices.ContainsFunc(list, func(x SenderBlock) bool { return x.ID == id }) {
		suppressed = unblocked(ctx, t, id, now)
	}
	found := false
	err := t.change(ctx, now, func(blocks []SenderBlock) ([]SenderBlock, error) {
		before := len(blocks)
		blocks = slices.DeleteFunc(blocks, func(x SenderBlock) bool { return x.ID == id })
		found = len(blocks) < before
		return blocks, nil
	})
	if err == nil && found && suppressed != nil {
		err = fmt.Errorf("%w: %w", ErrUnblockNotRecorded, suppressed)
	}
	return found, err
}

func (t tableSenders) Reject(ctx context.Context, auth Authentication, own []string, now time.Time) ([]SenderBlock, error) {
	return reject(ctx, t, auth, own, now)
}

func (t tableSenders) Accepted(ctx context.Context, auth Authentication, now time.Time) error {
	return accepted(ctx, t, auth, now)
}

// Status has no file to be damaged: Damaged reports an unreadable table, and ResetAt
// stays nil.
func (t tableSenders) Status(ctx context.Context, now time.Time) EvidenceStatus {
	var s EvidenceStatus
	if blocks, err := t.List(ctx, now); err == nil {
		s.AutomaticFull = automaticFull(blocks)
	}
	var good int
	var since sql.NullInt64
	if err := t.db.QueryRowContext(ctx, `SELECT count(*), min(last) FROM sender_evidence WHERE kind = 'good'`).Scan(&good, &since); err != nil {
		s.Damaged = true
		return s
	}
	s.GoodFull = good >= MaxGoodDomains
	if since.Valid {
		from := since.Int64 + domainWarmUp.Milliseconds()
		s.DomainBlocksFrom = &from
	}
	return s
}

func (t tableSenders) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (t tableSenders) change(ctx context.Context, now time.Time, edit func([]SenderBlock) ([]SenderBlock, error)) error {
	return t.tx(ctx, func(tx *sql.Tx) error { return changeTx(ctx, tx, now, edit) })
}

// changeTx is the file's read-modify-write inside tx: edit sees the blocks in force, and
// only rows that changed are written. Expired rows are dropped, as the file drops them.
func changeTx(ctx context.Context, tx *sql.Tx, now time.Time, edit func([]SenderBlock) ([]SenderBlock, error)) error {
	all, err := allBlocks(ctx, tx)
	if err != nil {
		return err
	}
	old := map[string]SenderBlock{}
	for _, b := range all {
		old[b.ID] = b
	}
	next, err := edit(inForce(slices.Clone(all), now))
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, b := range next {
		kept[b.ID] = true
		if o, ok := old[b.ID]; ok && sameBlock(o, b) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sender_blocks(`+blockColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, value = excluded.value, until = excluded.until, source = excluded.source,
 level = excluded.level, created_at = excluded.created_at, actor = excluded.actor, reason = excluded.reason`,
			b.ID, b.Kind, b.Value, b.Until, b.Source, b.Level, b.CreatedAt, b.Actor, b.Reason); err != nil {
			return err
		}
	}
	for id := range old {
		if !kept[id] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sender_blocks WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameBlock(a, b SenderBlock) bool {
	return a.Kind == b.Kind && a.Value == b.Value && (a.Until == nil) == (b.Until == nil) && (a.Until == nil || *a.Until == *b.Until) &&
		a.Source == b.Source && a.Level == b.Level && a.CreatedAt == b.CreatedAt && a.Actor == b.Actor && a.Reason == b.Reason
}

// knownGood is Accepted's one-row read before any write.
func (t tableSenders) knownGood(ctx context.Context, id string) bool {
	var n int
	err := t.db.QueryRowContext(ctx, `SELECT count(*) FROM sender_evidence WHERE kind = 'good' AND id = ?`, id).Scan(&n)
	return err == nil && n > 0
}

// edit is the file's Evidence.change row by row: the clean-period and expiry cleanup,
// the edit, then the same caps and eviction order (hits-only records first, then the
// oldest), all in one transaction.
func (t tableSenders) edit(ctx context.Context, now int64, fn func(evidenceState) error) error {
	return t.tx(ctx, func(tx *sql.Tx) error {
		st := &tableState{ctx: ctx, tx: tx}
		st.exec(`DELETE FROM sender_evidence WHERE kind IN ('address', 'domain') AND last <= ?`, now-cleanPeriod.Milliseconds())
		st.exec(`DELETE FROM sender_evidence WHERE kind = 'unblocked' AND last <= ?`, now)
		if st.err != nil {
			return st.err
		}
		if err := fn(st); err != nil {
			return err
		}
		for _, kind := range []string{"address", "domain", "unblocked"} {
			if over := st.count(`kind = ?`, kind) - MaxEvidence; over > 0 {
				st.exec(`DELETE FROM sender_evidence WHERE kind = ? AND id IN (SELECT id FROM sender_evidence WHERE kind = ?
 ORDER BY CASE WHEN level > 0 THEN 1 ELSE 0 END, last LIMIT ?)`, kind, kind, over)
			}
		}
		return st.err
	})
}

// tableState is evidenceState over sender_evidence rows: one row per address, domain,
// good domain (ref is its crude parent, last its first acceptance) and unblock (last is
// the end of suppression). goodSince is the earliest good row: the first acceptance is
// always recorded and good rows are never evicted. The first error sticks and fails the
// transaction.
type tableState struct {
	ctx context.Context
	tx  *sql.Tx
	err error
}

func (s *tableState) exec(query string, args ...any) {
	if s.err == nil {
		_, s.err = s.tx.ExecContext(s.ctx, query, args...)
	}
}

func (s *tableState) count(where string, args ...any) int {
	var n int
	if s.err == nil {
		s.err = s.tx.QueryRowContext(s.ctx, `SELECT count(*) FROM sender_evidence WHERE `+where, args...).Scan(&n)
	}
	return n
}

func (s *tableState) last(kind, id string) int64 {
	var at int64
	if s.err == nil {
		err := s.tx.QueryRowContext(s.ctx, `SELECT last FROM sender_evidence WHERE kind = ? AND id = ?`, kind, id).Scan(&at)
		if !errors.Is(err, sql.ErrNoRows) {
			s.err = err
		}
	}
	return at
}

func kindOf(address bool) string {
	if address {
		return "address"
	}
	return "domain"
}

func (s *tableState) record(address bool, id string) evidenceRecord {
	var r evidenceRecord
	var hits string
	if s.err != nil {
		return r
	}
	err := s.tx.QueryRowContext(s.ctx, `SELECT ref, hits, level, last, blocked FROM sender_evidence WHERE kind = ? AND id = ?`, kindOf(address), id).
		Scan(&r.Domain, &hits, &r.Level, &r.Last, &r.Blocked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		s.err = err
	case json.Unmarshal([]byte(hits), &r.Hits) != nil:
		s.err = errEvidence
	}
	return r
}

func (s *tableState) setRecord(address bool, id string, r evidenceRecord) {
	hits, _ := json.Marshal(r.Hits)
	s.upsert(kindOf(address), id, r.Domain, string(hits), r.Level, r.Last, r.Blocked)
}

func (s *tableState) upsert(kind, id, ref, hits string, level int, last, blocked int64) {
	s.exec(`INSERT INTO sender_evidence(kind, id, ref, hits, level, last, blocked) VALUES(?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(kind, id) DO UPDATE SET ref = excluded.ref, hits = excluded.hits, level = excluded.level, last = excluded.last, blocked = excluded.blocked`,
		kind, id, ref, hits, level, last, blocked)
}

func (s *tableState) blockedSince(domainID string, since int64) int {
	return s.count(`kind = 'address' AND ref = ? AND blocked > ?`, domainID, since)
}

func (s *tableState) good(id string) bool       { return s.count(`kind = 'good' AND id = ?`, id) > 0 }
func (s *tableState) goodCount() int            { return s.count(`kind = 'good'`) }
func (s *tableState) parent(id string) int      { return s.count(`kind = 'good' AND ref = ?`, id) }
func (s *tableState) setGoodSince(int64)        {}
func (s *tableState) unblocked(id string) int64 { return s.last("unblocked", id) }

func (s *tableState) goodSince() int64 {
	var at sql.NullInt64
	if s.err == nil {
		s.err = s.tx.QueryRowContext(s.ctx, `SELECT min(last) FROM sender_evidence WHERE kind = 'good'`).Scan(&at)
	}
	return at.Int64
}

func (s *tableState) addGood(id, parent string, at int64) {
	s.upsert("good", id, parent, "null", 0, at, 0)
}

func (s *tableState) setUnblocked(id string, until int64) {
	s.upsert("unblocked", id, "", "null", 0, until, 0)
}

func (s *tableState) put(ctx context.Context, b SenderBlock, own []string, now time.Time) (SenderBlock, error) {
	if s.err != nil {
		return SenderBlock{}, s.err
	}
	// A refused block leaves the evidence write to go on, as with the files; a failed
	// row write must fail the whole transaction, never commit half a list.
	wrote := false
	block, err := put(ctx, func(ctx context.Context, now time.Time, edit func([]SenderBlock) ([]SenderBlock, error)) error {
		return changeTx(ctx, s.tx, now, func(blocks []SenderBlock) ([]SenderBlock, error) {
			next, err := edit(blocks)
			wrote = err == nil
			return next, err
		})
	}, b, own, now)
	if err != nil && wrote {
		s.err = err
	}
	return block, err
}
