package ingress

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

const (
	// EvidenceFile holds automatic-block evidence beside sender-blocks.json.
	// Every key is a BlockID, every value a count or time: no addresses, no
	// message content.
	EvidenceFile = "sender-evidence.json"
	// EvidenceDamagedFile is the last unreadable evidence file, set aside so
	// counting restarts; backups skip it.
	EvidenceDamagedFile = "sender-evidence.damaged.json"
)

const (
	autoHits      = 5              // reject verdicts from one identity ...
	autoWindow    = time.Hour      // ... within this window block it
	domainHits    = 5              // distinct automatically blocked addresses ...
	domainWindow  = 24 * time.Hour // ... within this window block a never-good domain
	cleanPeriod   = 30 * 24 * time.Hour
	UnblockWindow = cleanPeriod // a manual unblock suppresses automatic re-blocking
	// Automatic domain blocks wait until authenticated acceptances have been
	// recorded this long, so "never accepted mail from it" means something.
	domainWarmUp   = 30 * 24 * time.Hour
	MaxEvidence    = 4096 // address, domain and unblock records, each
	MaxGoodDomains = 50000
	// ponytail: a crude registrable parent, the last two labels, since no
	// public suffix list is installed. Its ceiling: every domain under a
	// two-label public suffix (co.uk, com.au) shares one parent, so past 50
	// of them further ones there stay unprotected. Upgrade: x/net/publicsuffix.
	maxGoodPerParent = 50
	// MaxEvidenceBytes bounds the file. Records are fixed-width IDs and
	// integers, so the caps above keep it under it (TestEvidenceWorstCaseSize).
	MaxEvidenceBytes = 8 << 20
)

// Cooldown per escalation level 1..3.
var autoCooldown = [...]time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

var (
	ErrAutomaticFull      = errors.New("automatic blocks fill their half of the block list; add a manual (domain) block or wait for some to expire")
	ErrUnblockNotRecorded = errors.New("block removed, but automatic re-blocking could not be suppressed, so it may come back; check receiving storage")
	errEvidence           = errors.New("sender evidence is malformed or oversized")
	errManualCovers       = errors.New("a manual block covers this sender")
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
	Domain  string  `json:"domain,omitempty"` // BlockID of an address's domain
	Hits    []int64 `json:"hits"`             // counted reject verdicts inside autoWindow, Unix ms
	Level   int     `json:"level"`            // level of the last automatic block made, 0 none
	Last    int64   `json:"last"`             // latest counted verdict or block attempt
	Blocked int64   `json:"blocked"`          // latest automatic block made, 0 none
}

type evidenceDoc struct {
	Version   int                       `json:"version"`
	Addresses map[string]evidenceRecord `json:"addresses"` // address BlockID → record
	Domains   map[string]evidenceRecord `json:"domains"`   // domain BlockID → record
	Good      map[string]int64          `json:"good"`      // domain BlockID → first authenticated acceptance; never evicted
	Parents   map[string]int            `json:"parents"`   // crude parent BlockID → good domains under it
	GoodSince int64                     `json:"goodSince"` // first authenticated acceptance recorded, 0 none
	Unblocked map[string]int64          `json:"unblocked"` // BlockID → end of suppression
	ResetAt   int64                     `json:"resetAt"`   // a damaged file was set aside then, 0 never
}

// Evidence is the automatic-block state in an existing receiving directory,
// under its own lock. Lock order: evidence, then blocks; never the reverse.
type Evidence struct{ dir string }

func NewEvidence(receivingDir string) Evidence { return Evidence{dir: receivingDir} }

func (e Evidence) path() string { return filepath.Join(e.dir, EvidenceFile) }

func validID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 8 && hex.EncodeToString(raw) == id
}

// ParseEvidence is the file rule; backups skip a file failing it.
func ParseEvidence(raw []byte) error {
	_, err := parseEvidence(raw)
	return err
}

func parseEvidence(raw []byte) (evidenceDoc, error) {
	var doc evidenceDoc
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > MaxEvidenceBytes || d.Decode(&doc) != nil || d.Decode(new(any)) != io.EOF || doc.Version != 1 || doc.GoodSince < 0 || doc.ResetAt < 0 ||
		len(doc.Addresses) > MaxEvidence || len(doc.Domains) > MaxEvidence || len(doc.Unblocked) > MaxEvidence || len(doc.Good) > MaxGoodDomains || len(doc.Parents) > MaxGoodDomains {
		return evidenceDoc{}, errEvidence
	}
	records := func(records map[string]evidenceRecord, address bool) bool {
		for id, r := range records {
			if !validID(id) || address != validID(r.Domain) || !address && r.Domain != "" || len(r.Hits) >= autoHits || r.Level < 0 || r.Level > len(autoCooldown) ||
				r.Last < 1 || r.Blocked < 0 || slices.ContainsFunc(r.Hits, func(t int64) bool { return t < 1 }) {
				return false
			}
		}
		return true
	}
	times := func(m map[string]int64) bool {
		for id, t := range m {
			if !validID(id) || t < 1 {
				return false
			}
		}
		return true
	}
	for id, n := range doc.Parents {
		if !validID(id) || n < 1 || n > maxGoodPerParent {
			return evidenceDoc{}, errEvidence
		}
	}
	if !records(doc.Addresses, true) || !records(doc.Domains, false) || !times(doc.Good) || !times(doc.Unblocked) {
		return evidenceDoc{}, errEvidence
	}
	return doc, nil
}

func (e Evidence) load() (evidenceDoc, error) {
	f, err := os.Open(e.path())
	if errors.Is(err, os.ErrNotExist) {
		return evidenceDoc{Version: 1}, nil
	}
	if err != nil {
		return evidenceDoc{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxEvidenceBytes+1))
	if err != nil {
		return evidenceDoc{}, err
	}
	return parseEvidence(raw)
}

// change is the locked read-modify-write; like Blocks it never creates the
// receiving directory. A damaged or oversized file is renamed to
// EvidenceDamagedFile and counting restarts (ResetAt): this is heuristic
// state, and refusing every write would leave abuse unblocked until someone
// edited files. Expired and clean records are dropped first.
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
	if errors.Is(err, errEvidence) {
		if err := os.Rename(e.path(), filepath.Join(e.dir, EvidenceDamagedFile)); err != nil {
			return err
		}
		slog.Error("receiving sender evidence", "actor", "automatic", "task_id", "native-receiving", "action", "reset", "target", "sender-evidence", "result", "damaged-file-set-aside", "correlation_id", EvidenceDamagedFile)
		doc, err = evidenceDoc{Version: 1, ResetAt: now}, nil
	}
	if err != nil {
		return err
	}
	for _, records := range []map[string]evidenceRecord{doc.Addresses, doc.Domains} {
		for id, r := range records {
			if r.Last <= now-cleanPeriod.Milliseconds() {
				delete(records, id) // a clean period resets the level
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
	// Hits-only records go before any record holding a level (an active or
	// recent block), so a flood of one-off identities cannot reset an abuser.
	keep := func(r evidenceRecord) int64 { return r.Last + int64(min(r.Level, 1))<<61 }
	trim(doc.Addresses, MaxEvidence, keep)
	trim(doc.Domains, MaxEvidence, keep)
	trim(doc.Unblocked, MaxEvidence, func(until int64) int64 { return until })
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if len(raw) > MaxEvidenceBytes {
		return fmt.Errorf("%w: %d bytes", errEvidence, len(raw))
	}
	return fsutil.AtomicWriteFile(e.path(), raw, 0o600)
}

// ponytail: an O(n) scan per eviction, one eviction per write in practice.
func trim[V any](m map[string]V, limit int, keep func(V) int64) {
	for len(m) > limit {
		victim := ""
		for id, v := range m {
			if victim == "" || keep(v) < keep(m[victim]) {
				victim = id
			}
		}
		delete(m, victim)
	}
}

// Reject counts one reject verdict and returns the automatic blocks it made.
// Only an authenticated identity (Identity) counts; own domains, the null
// sender and anything under a manual block or a recent manual unblock never
// do. 5 counted verdicts within an hour block the address for 1 h, 24 h,
// then 7 d per repeat. 5 distinct addresses blocked on one domain within a
// day, since its last domain block, also block the domain, unless
// authenticated mail was accepted from it or acceptances have been recorded
// for less than domainWarmUp.
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
	addressID, domainID := BlockID("address", address), BlockID("domain", domain)
	var made []SenderBlock
	var putErr error
	// escalate puts the next level's block; the level climbs only when the
	// block was actually made.
	escalate := func(r *evidenceRecord, kind, value string) {
		level := min(r.Level+1, len(autoCooldown))
		until := now.Add(autoCooldown[level-1]).UnixMilli()
		r.Hits, r.Last = nil, ms
		block, err := NewBlocks(e.dir).Put(ctx, SenderBlock{Kind: kind, Value: value, Until: &until, Source: "automatic", Level: level, Actor: "automatic", Reason: "abuse"}, own, now)
		switch {
		case err == nil:
			r.Level, r.Blocked = level, ms
			made = append(made, block)
		case !errors.Is(err, errManualCovers):
			putErr = err
		}
	}
	err := e.change(ctx, ms, func(doc *evidenceDoc) error {
		if doc.Unblocked[addressID] > ms {
			return nil
		}
		if doc.Addresses == nil {
			doc.Addresses = map[string]evidenceRecord{}
		}
		r := doc.Addresses[addressID]
		r.Domain = domainID
		r.Hits = append(slices.DeleteFunc(r.Hits, func(t int64) bool { return t <= ms-autoWindow.Milliseconds() }), ms)
		r.Last = ms
		if len(r.Hits) >= autoHits {
			escalate(&r, "address", address)
		}
		doc.Addresses[addressID] = r
		if r.Blocked != ms {
			return nil
		}
		d := doc.Domains[domainID]
		since := max(ms-domainWindow.Milliseconds(), d.Blocked)
		count := 0
		for _, x := range doc.Addresses {
			if x.Domain == domainID && x.Blocked > since {
				count++
			}
		}
		_, good := doc.Good[domainID]
		if count < domainHits || good || doc.GoodSince == 0 || ms < doc.GoodSince+domainWarmUp.Milliseconds() || doc.Unblocked[domainID] > ms {
			return nil
		}
		if doc.Domains == nil {
			doc.Domains = map[string]evidenceRecord{}
		}
		escalate(&d, "domain", domain)
		doc.Domains[domainID] = d
		return nil
	})
	if err != nil {
		return made, err
	}
	return made, putErr
}

// Accepted records the domain of an authenticated identity (Identity) whose
// mail this deployment accepted, so it is never blocked automatically as a
// domain. Unauthenticated envelope domains are never recorded: anyone can
// forge them. Recorded domains are never evicted, so a flood of authenticated
// throwaway domains cannot unprotect a real one: past MaxGoodDomains, or
// maxGoodPerParent under one parent, new domains are simply not recorded.
func (e Evidence) Accepted(ctx context.Context, auth Authentication, now time.Time) error {
	address, ok := auth.Identity()
	if !ok {
		return nil
	}
	domain := address[strings.LastIndexByte(address, '@')+1:]
	labels := strings.Split(domain, ".")
	id, parent, ms := BlockID("domain", domain), BlockID("domain", strings.Join(labels[max(len(labels)-2, 0):], ".")), now.UnixMilli()
	// ponytail: parses the whole file per authenticated acceptance, fine at
	// household volume; past that, cache it by file inode and mtime.
	if doc, err := e.load(); err == nil && doc.Good[id] > 0 {
		return nil
	}
	return e.change(ctx, ms, func(doc *evidenceDoc) error {
		if doc.GoodSince == 0 {
			doc.GoodSince = ms
		}
		if doc.Good[id] > 0 || len(doc.Good) >= MaxGoodDomains || doc.Parents[parent] >= maxGoodPerParent {
			return nil
		}
		if doc.Good == nil {
			doc.Good = map[string]int64{}
		}
		if doc.Parents == nil {
			doc.Parents = map[string]int{}
		}
		doc.Good[id] = ms
		doc.Parents[parent]++
		return nil
	})
}

// unblocked records an administrator's unblock of block id: automatic blocks
// of exactly that address or domain are suppressed for UnblockWindow.
// Nothing counts meanwhile, so its escalation has reset when it ends.
func (e Evidence) unblocked(ctx context.Context, id string, now time.Time) error {
	ms := now.UnixMilli()
	return e.change(ctx, ms, func(doc *evidenceDoc) error {
		if doc.Unblocked == nil {
			doc.Unblocked = map[string]int64{}
		}
		doc.Unblocked[id] = ms + UnblockWindow.Milliseconds()
		return nil
	})
}

// EvidenceStatus is what an administrator needs to trust automatic blocks.
type EvidenceStatus struct {
	// Damaged: the current file is unreadable. A malformed or oversized one
	// is set aside by the next write; any other read failure needs storage
	// repair.
	Damaged bool `json:"damaged"`
	// ResetAt: evidence restarted then after damage (EvidenceDamagedFile).
	ResetAt *int64 `json:"resetAt"`
	// DomainBlocksFrom: automatic domain blocks are possible from then;
	// null until an authenticated acceptance has been recorded.
	DomainBlocksFrom *int64 `json:"domainBlocksFrom"`
	// GoodFull: no more accepted domains are recorded; domains not yet
	// recorded stay domain-blockable.
	GoodFull bool `json:"goodFull"`
	// AutomaticFull: automatic blocks are within one block of their half of
	// the list, so new ones are refused until some expire. A manual domain
	// block covers a flood of addresses.
	AutomaticFull bool `json:"automaticFull"`
}

func (e Evidence) Status(now time.Time) EvidenceStatus {
	var s EvidenceStatus
	if blocks, err := NewBlocks(e.dir).List(now); err == nil {
		s.AutomaticFull = automaticFull(blocks)
	}
	doc, err := e.load()
	if err != nil {
		s.Damaged = true
		return s
	}
	s.GoodFull = len(doc.Good) >= MaxGoodDomains
	if doc.ResetAt > 0 {
		s.ResetAt = &doc.ResetAt
	}
	if doc.GoodSince > 0 {
		from := doc.GoodSince + domainWarmUp.Milliseconds()
		s.DomainBlocksFrom = &from
	}
	return s
}
