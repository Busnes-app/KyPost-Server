package ingress

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// EvidenceFile holds automatic-block evidence beside sender-blocks.json:
// addresses, domains, counts and times, never message content.
const EvidenceFile = "sender-evidence.json"

const (
	autoHits       = 5              // reject verdicts from one identity ...
	autoWindow     = time.Hour      // ... within this window block it
	domainHits     = 5              // distinct automatically blocked addresses ...
	domainWindow   = 24 * time.Hour // ... within this window block a never-good domain
	cleanPeriod    = 30 * 24 * time.Hour
	UnblockWindow  = cleanPeriod // a manual unblock suppresses automatic re-blocking
	MaxEvidence    = 4096        // address, domain and unblock records, each
	MaxGoodDomains = 10000
	maxEvidenceRaw = 8 << 20
)

// Cooldown per escalation level 1..3.
var autoCooldown = [...]time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

var (
	ErrGoodDomainsFull = errors.New("accepted-domain record is full; automatic domain blocks are disabled")
	errEvidence        = errors.New("sender evidence is malformed; restore it from backup or remove it")
	errManualCovers    = errors.New("a manual block covers this sender")
)

// Authentication is the receiving MTA's own result for one message. Callers
// fill it from their scanner, never from headers in the message.
type Authentication struct {
	Sender string   // envelope MAIL FROM
	From   string   // the message's single RFC 5322 From address; "" if none or several
	SPF    bool     // SPF passed for the envelope domain
	DKIM   []string // d= of each verified DKIM signature, LowerASCII
}

// Identity is the address a message may count against: envelope sender equal
// to From, SPF pass, and a DKIM signature whose d= is exactly the address's
// domain (DMARC strict alignment; a parent or child domain does not count).
func (a Authentication) Identity() (string, bool) {
	sender := cfreceiving.LowerASCII(a.Sender)
	at := strings.LastIndexByte(sender, '@')
	if !a.SPF || at < 1 || !cfreceiving.ValidSender(a.Sender) || sender != cfreceiving.LowerASCII(a.From) || !slices.Contains(a.DKIM, sender[at+1:]) {
		return "", false
	}
	return sender, true
}

type evidenceRecord struct {
	Hits    []int64 `json:"hits"`    // counted reject verdicts inside autoWindow, Unix ms
	Level   int     `json:"level"`   // last automatic block level, 0 none
	Last    int64   `json:"last"`    // latest counted verdict or block
	Blocked int64   `json:"blocked"` // latest automatic block, 0 none
}

type evidenceDoc struct {
	Version   int                       `json:"version"`
	Addresses map[string]evidenceRecord `json:"addresses"`
	Domains   map[string]evidenceRecord `json:"domains"`
	Good      []string                  `json:"good"`      // envelope domains mail was accepted from; sorted, never pruned
	Unblocked map[string]int64          `json:"unblocked"` // BlockID → end of suppression
}

// Evidence is the automatic-block state in an existing receiving directory,
// under its own lock. Lock order: evidence, then blocks; never the reverse.
type Evidence struct{ dir string }

func NewEvidence(receivingDir string) Evidence { return Evidence{dir: receivingDir} }

func (e Evidence) path() string { return filepath.Join(e.dir, EvidenceFile) }

// ParseEvidence is the file rule; backups validate collected bytes with it.
func ParseEvidence(raw []byte) error {
	_, err := parseEvidence(raw)
	return err
}

func parseEvidence(raw []byte) (evidenceDoc, error) {
	var doc evidenceDoc
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > maxEvidenceRaw || d.Decode(&doc) != nil || d.Decode(new(any)) != io.EOF || doc.Version != 1 ||
		len(doc.Addresses) > MaxEvidence || len(doc.Domains) > MaxEvidence || len(doc.Unblocked) > MaxEvidence || len(doc.Good) > MaxGoodDomains ||
		!slices.IsSorted(doc.Good) || len(slices.Compact(slices.Clone(doc.Good))) != len(doc.Good) {
		return evidenceDoc{}, errEvidence
	}
	valid := func(kind string, records map[string]evidenceRecord) bool {
		for k, r := range records {
			if v, err := NormalizeBlock(kind, k); err != nil || v != k || len(r.Hits) >= autoHits || r.Level < 0 || r.Level > len(autoCooldown) || r.Last < 1 || r.Blocked < 0 ||
				slices.ContainsFunc(r.Hits, func(t int64) bool { return t < 1 }) {
				return false
			}
		}
		return true
	}
	for id, until := range doc.Unblocked {
		if raw, err := hex.DecodeString(id); err != nil || len(raw) != 8 || hex.EncodeToString(raw) != id || until < 1 {
			return evidenceDoc{}, errEvidence
		}
	}
	for _, g := range doc.Good {
		if v, err := NormalizeBlock("domain", g); err != nil || v != g {
			return evidenceDoc{}, errEvidence
		}
	}
	if !valid("address", doc.Addresses) || !valid("domain", doc.Domains) {
		return evidenceDoc{}, errEvidence
	}
	return doc, nil
}

func (e Evidence) load() (evidenceDoc, error) {
	raw, err := os.ReadFile(e.path())
	if errors.Is(err, os.ErrNotExist) {
		return evidenceDoc{Version: 1}, nil
	}
	if err != nil {
		return evidenceDoc{}, err
	}
	return parseEvidence(raw)
}

// change is the locked read-modify-write; like Blocks it never creates the
// receiving directory. Expired and clean records are dropped first.
func (e Evidence) change(ctx context.Context, now int64, edit func(*evidenceDoc) error) error {
	if info, err := os.Lstat(e.dir); err != nil || !info.IsDir() {
		return ErrBlockStore
	}
	release, err := fsutil.LockFileContext(ctx, e.path())
	if err != nil {
		return err
	}
	defer release()
	doc, err := e.load()
	if err != nil {
		return err
	}
	for _, records := range []map[string]evidenceRecord{doc.Addresses, doc.Domains} {
		for k, r := range records {
			if r.Last <= now-cleanPeriod.Milliseconds() {
				delete(records, k) // a clean period resets the level
			}
		}
	}
	for id, until := range doc.Unblocked {
		if until <= now {
			delete(doc.Unblocked, id)
		}
	}
	if err := edit(&doc); err != nil {
		return err
	}
	// ponytail: evicts the oldest record past the cap with an O(n) scan per
	// write; an attacker with that many authenticated identities can reset an
	// old escalation. Upgrade: a heap, or a larger cap if hosts can afford it.
	trim(doc.Addresses, func(r evidenceRecord) int64 { return r.Last })
	trim(doc.Domains, func(r evidenceRecord) int64 { return r.Last })
	trim(doc.Unblocked, func(until int64) int64 { return until })
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(e.path(), raw, 0o600)
}

func trim[V any](m map[string]V, age func(V) int64) {
	for len(m) > MaxEvidence {
		oldest := ""
		for k, v := range m {
			if oldest == "" || age(v) < age(m[oldest]) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
}

// Reject counts one reject verdict and returns the automatic blocks it made.
// Only an authenticated identity (Identity) counts; own domains, the null
// sender and anything under a manual block or a recent manual unblock never
// do. 5 counted verdicts within an hour block the address for 1 h, 24 h,
// then 7 d per repeat. 5 distinct addresses blocked on one domain within a
// day, since its last domain block, also block a domain this deployment has
// never accepted mail from, unless the accepted-domain record is full.
func (e Evidence) Reject(ctx context.Context, auth Authentication, own []string, now time.Time) ([]SenderBlock, error) {
	address, ok := auth.Identity()
	if !ok {
		return nil, nil
	}
	domain := address[strings.LastIndexByte(address, '@')+1:]
	if slices.Contains(own, domain) {
		return nil, nil
	}
	ms := now.UnixMilli()
	var made []SenderBlock
	var putErr error
	put := func(r *evidenceRecord, kind, value string) {
		block, err := e.escalate(ctx, r, kind, value, own, now)
		if err == nil {
			made = append(made, block)
		} else if !errors.Is(err, errManualCovers) {
			putErr = err
		}
	}
	err := e.change(ctx, ms, func(doc *evidenceDoc) error {
		if doc.Unblocked[BlockID("address", address)] > ms {
			return nil
		}
		if doc.Addresses == nil {
			doc.Addresses = map[string]evidenceRecord{}
		}
		r := doc.Addresses[address]
		r.Hits = append(slices.DeleteFunc(r.Hits, func(t int64) bool { return t <= ms-autoWindow.Milliseconds() }), ms)
		r.Last = ms
		if len(r.Hits) >= autoHits {
			put(&r, "address", address)
		}
		doc.Addresses[address] = r
		if r.Blocked != ms {
			return nil
		}
		_, good := slices.BinarySearch(doc.Good, domain)
		d := doc.Domains[domain]
		since := max(ms-domainWindow.Milliseconds(), d.Blocked)
		count := 0
		for k, x := range doc.Addresses {
			if x.Blocked > since && strings.HasSuffix(k, "@"+domain) {
				count++
			}
		}
		if count < domainHits || good || len(doc.Good) >= MaxGoodDomains || doc.Unblocked[BlockID("domain", domain)] > ms {
			return nil
		}
		if doc.Domains == nil {
			doc.Domains = map[string]evidenceRecord{}
		}
		put(&d, "domain", domain)
		doc.Domains[domain] = d
		return nil
	})
	if err != nil {
		return made, err
	}
	return made, putErr
}

// escalate records the next level on r and puts its automatic block. The
// level is kept even when Put fails, so a repeat still escalates; Blocked
// marks only a block actually made.
func (e Evidence) escalate(ctx context.Context, r *evidenceRecord, kind, value string, own []string, now time.Time) (SenderBlock, error) {
	r.Level = min(r.Level+1, len(autoCooldown))
	r.Hits, r.Last = nil, now.UnixMilli()
	until := now.Add(autoCooldown[r.Level-1]).UnixMilli()
	block, err := NewBlocks(e.dir).Put(ctx, SenderBlock{Kind: kind, Value: value, Until: &until, Source: "automatic", Level: r.Level, Actor: "automatic", Reason: "abuse"}, own, now)
	if err == nil {
		r.Blocked = r.Last
	}
	return block, err
}

// Accepted records the envelope domain of mail this deployment accepted
// (any verdict but reject), so it is never blocked automatically as a domain.
// Domains are never pruned; a full record returns ErrGoodDomainsFull and
// disables automatic domain blocks rather than forgetting one.
func (e Evidence) Accepted(ctx context.Context, sender string, now time.Time) error {
	at := strings.LastIndexByte(sender, '@')
	if sender == "" || at < 0 {
		return nil
	}
	domain, err := NormalizeBlock("domain", sender[at+1:])
	if err != nil {
		return nil
	}
	// ponytail: parses the whole file on every accepted message, fine at
	// household volume; past that, cache Good by file inode and mtime.
	if doc, err := e.load(); err == nil {
		if _, found := slices.BinarySearch(doc.Good, domain); found {
			return nil
		}
	}
	return e.change(ctx, now.UnixMilli(), func(doc *evidenceDoc) error {
		i, found := slices.BinarySearch(doc.Good, domain)
		switch {
		case found:
			return nil
		case len(doc.Good) >= MaxGoodDomains:
			return ErrGoodDomainsFull
		}
		doc.Good = slices.Insert(doc.Good, i, domain)
		return nil
	})
}

// unblocked records an administrator's unblock: automatic blocks of exactly
// this kind/value are suppressed for UnblockWindow. Nothing counts meanwhile,
// so its escalation has reset (cleanPeriod) by the time the window ends.
func (e Evidence) unblocked(ctx context.Context, kind, value string, now time.Time) error {
	ms := now.UnixMilli()
	return e.change(ctx, ms, func(doc *evidenceDoc) error {
		if doc.Unblocked == nil {
			doc.Unblocked = map[string]int64{}
		}
		doc.Unblocked[BlockID(kind, value)] = ms + UnblockWindow.Milliseconds()
		return nil
	})
}
