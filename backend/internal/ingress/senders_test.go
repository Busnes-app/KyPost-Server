package ingress

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// home is one sender store under test: the files (single mode) or tenancy.db's tables
// (multi mode; PostgreSQL when KYPOST_TEST_DATABASE_URL is set). The sender-block and
// evidence tests run against both through it.
type home struct {
	SenderStore
	name string
	dir  string  // the receiving directory; the table home gets one too and must leave it empty
	db   *sql.DB // nil for files
}

// eachHome runs fn once per home.
func eachHome(t *testing.T, fn func(t *testing.T, h *home)) {
	for _, name := range []string{"files", "tables"} {
		t.Run(name, func(t *testing.T) { fn(t, newHome(t, name)) })
	}
}

func newHome(t *testing.T, name string) *home {
	t.Helper()
	h := &home{name: name, dir: t.TempDir()}
	if name == "files" {
		h.SenderStore = FileSenders(h.dir)
		return h
	}
	db, err := tenancy.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	h.db, h.SenderStore = db, TableSenders(db)
	t.Cleanup(func() {
		_ = db.Close()
		if entries, err := os.ReadDir(h.dir); err != nil || len(entries) != 0 {
			t.Error("the table home touched the receiving directory", entries, err)
		}
	})
	return h
}

// fresh is an empty home of the same kind.
func (h *home) fresh(t *testing.T) *home { return newHome(t, h.name) }

func (h *home) files() bool { return h.db == nil }

func (h *home) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// insertRows inserts rows in batches of 200 (one statement each).
func (h *home) insertRows(t *testing.T, table, cols string, rows [][]any) {
	t.Helper()
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", strings.Count(cols, ",")+1), ", ") + ")"
	for batch := range slices.Chunk(rows, 200) {
		var args []any
		for _, r := range batch {
			args = append(args, r...)
		}
		h.exec(t, `INSERT INTO `+table+`(`+cols+`) VALUES `+strings.TrimSuffix(strings.Repeat(marks+", ", len(batch)), ", "), args...)
	}
}

// seedBlocks replaces the whole list with blocks, unchecked.
func (h *home) seedBlocks(t *testing.T, blocks []SenderBlock) {
	t.Helper()
	if h.files() {
		raw, _ := json.Marshal(map[string]any{"version": 1, "blocks": blocks})
		if err := os.WriteFile(filepath.Join(h.dir, BlocksFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	h.exec(t, `DELETE FROM sender_blocks`)
	var rows [][]any
	for _, b := range blocks {
		rows = append(rows, []any{b.ID, b.Kind, b.Value, b.Until, b.Source, b.Level, b.CreatedAt, b.Actor, b.Reason})
	}
	h.insertRows(t, "sender_blocks", blockColumns, rows)
}

// blocksRaw is the stored list, for unchanged-or-not comparisons and content checks.
func (h *home) blocksRaw(t *testing.T) []byte {
	t.Helper()
	if h.files() {
		raw, _ := os.ReadFile(filepath.Join(h.dir, BlocksFile))
		return raw
	}
	blocks, err := readBlocks(context.Background(), h.db, ` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(blocks)
	return raw
}

// corruptBlock stores a block for the domain evil.test that breaks the stored-entry rule.
func (h *home) corruptBlock(t *testing.T) {
	t.Helper()
	if h.files() {
		if err := os.WriteFile(filepath.Join(h.dir, BlocksFile), []byte(`{"version":1,"blocks":[{"id":"x","kind":"domain","value":"Evil.test","until":null,"source":"manual","level":0,"createdAt":1,"actor":"a","reason":"spam"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	h.exec(t, `INSERT INTO sender_blocks(`+blockColumns+`) VALUES(?, 'domain', 'Evil.test', NULL, 'manual', 0, 1, 'a', 'spam')`, BlockID("domain", "evil.test"))
}

// evidence reads the stored evidence as the file's document.
func (h *home) evidence(t *testing.T) evidenceDoc {
	t.Helper()
	if h.files() {
		doc, err := NewEvidence(h.dir).load()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := evidenceDoc{Version: 1, Addresses: map[string]evidenceRecord{}, Domains: map[string]evidenceRecord{}, Good: map[string]int64{}, Parents: map[string]int{}, Unblocked: map[string]int64{}}
	rows, err := h.db.Query(`SELECT kind, id, ref, hits, level, last, blocked FROM sender_evidence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, hits string
		var r evidenceRecord
		if err := rows.Scan(&kind, &id, &r.Domain, &hits, &r.Level, &r.Last, &r.Blocked); err != nil || json.Unmarshal([]byte(hits), &r.Hits) != nil {
			t.Fatal(err, hits)
		}
		switch kind {
		case "address":
			doc.Addresses[id] = r
		case "domain":
			doc.Domains[id] = r
		case "good":
			doc.Good[id] = r.Last
			if r.Domain != "" {
				doc.Parents[r.Domain]++
			}
			if doc.GoodSince == 0 || r.Last < doc.GoodSince {
				doc.GoodSince = r.Last
			}
		case "unblocked":
			doc.Unblocked[id] = r.Last
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return doc
}

// seedEvidence replaces the stored evidence with doc. Tables derive GoodSince and
// Parents from the good rows, so a good row's parent is ref (empty here).
func (h *home) seedEvidence(t *testing.T, doc evidenceDoc) {
	t.Helper()
	if h.files() {
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(h.dir, EvidenceFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	h.exec(t, `DELETE FROM sender_evidence`)
	var rows [][]any
	record := func(kind string, records map[string]evidenceRecord) {
		for id, r := range records {
			hits, _ := json.Marshal(r.Hits)
			rows = append(rows, []any{kind, id, r.Domain, string(hits), r.Level, r.Last, r.Blocked})
		}
	}
	record("address", doc.Addresses)
	record("domain", doc.Domains)
	for id, at := range doc.Good {
		rows = append(rows, []any{"good", id, "", "null", 0, at, 0})
	}
	for id, until := range doc.Unblocked {
		rows = append(rows, []any{"unblocked", id, "", "null", 0, until, 0})
	}
	h.insertRows(t, "sender_evidence", "kind, id, ref, hits, level, last, blocked", rows)
}

// wroteEvidence reports whether any evidence is stored.
func (h *home) wroteEvidence(t *testing.T) bool {
	t.Helper()
	if h.files() {
		_, err := os.Stat(filepath.Join(h.dir, EvidenceFile))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM sender_evidence`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// evidenceRaw is every stored evidence byte, for content checks.
func (h *home) evidenceRaw(t *testing.T) []byte {
	t.Helper()
	if h.files() {
		raw, _ := os.ReadFile(filepath.Join(h.dir, EvidenceFile))
		return raw
	}
	return fmt.Appendf(nil, "%+v", h.evidence(t))
}
