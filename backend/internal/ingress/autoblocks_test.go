package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func authed(address string) Authentication {
	return Authentication{Sender: address, From: address, SPF: true, DKIM: []string{address[strings.LastIndexByte(address, '@')+1:]}}
}

// rejects counts n reject verdicts one minute apart from start and returns
// the blocks made.
func rejects(t *testing.T, e SenderStore, a Authentication, own []string, start time.Time, n int) []SenderBlock {
	t.Helper()
	var made []SenderBlock
	for i := range n {
		blocks, err := e.Reject(context.Background(), a, own, start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, blocks...)
	}
	return made
}

func blockedAt(t *testing.T, s SenderStore, sender string, at time.Time) bool {
	t.Helper()
	blocked, err := s.Blocked(context.Background(), sender, at)
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

// The design's offline qualification: five reject verdicts that pass SPF and
// DKIM for a shared domain, but whose envelope sender and From differ, block
// nobody, however often they repeat.
func TestAutomaticBlocksSharedDomainSpoofing(t *testing.T) { eachHome(t, testSharedDomainSpoofing) }

func testSharedDomainSpoofing(t *testing.T, e *home) {
	for round := range 5 {
		for i := range 5 {
			a := Authentication{Sender: fmt.Sprintf("env%d@shared.test", i), From: fmt.Sprintf("from%d@shared.test", i), SPF: true, DKIM: []string{"shared.test"}}
			if made, err := e.Reject(context.Background(), a, nil, t0.Add(time.Duration(round*5+i)*time.Second)); err != nil || len(made) != 0 {
				t.Fatal("spoofed identity blocked", made, err)
			}
		}
	}
	for i := range 5 {
		for _, sender := range []string{fmt.Sprintf("env%d@shared.test", i), fmt.Sprintf("from%d@shared.test", i)} {
			if blockedAt(t, e, sender, t0.Add(time.Minute)) {
				t.Fatal("blocked", sender)
			}
		}
	}
	if e.wroteEvidence(t) {
		t.Fatal("uncounted verdicts wrote evidence")
	}
}

func TestAutomaticBlocksEvidenceConditions(t *testing.T) { eachHome(t, testEvidenceConditions) }

func testEvidenceConditions(t *testing.T, h *home) {
	good := authed("alice@shared.test")
	if id, ok := good.Identity(); !ok || id != "alice@shared.test" {
		t.Fatal("authenticated identity refused")
	}
	// A-Z case folds on both sides, as the block matcher does.
	if id, ok := (Authentication{Sender: "Alice@Shared.TEST", From: "ALICE@shared.test", SPF: true, DKIM: []string{"shared.test"}}).Identity(); !ok || id != "alice@shared.test" {
		t.Fatal("ASCII case mismatch refused", id)
	}
	for name, a := range map[string]Authentication{
		"spf":            {Sender: "alice@shared.test", From: "alice@shared.test", DKIM: []string{"shared.test"}},
		"no dkim":        {Sender: "alice@shared.test", From: "alice@shared.test", SPF: true},
		"other dkim":     {Sender: "alice@shared.test", From: "alice@shared.test", SPF: true, DKIM: []string{"other.test"}},
		"parent dkim":    {Sender: "alice@mail.shared.test", From: "alice@mail.shared.test", SPF: true, DKIM: []string{"shared.test"}},
		"child dkim":     {Sender: "alice@shared.test", From: "alice@shared.test", SPF: true, DKIM: []string{"mail.shared.test"}},
		"from differs":   {Sender: "alice@shared.test", From: "bob@shared.test", SPF: true, DKIM: []string{"shared.test"}},
		"no from":        {Sender: "alice@shared.test", SPF: true, DKIM: []string{"shared.test"}},
		"null sender":    {From: "alice@shared.test", SPF: true, DKIM: []string{"shared.test"}},
		"non-ASCII case": {Sender: "é@shared.test", From: "É@shared.test", SPF: true, DKIM: []string{"shared.test"}},
		"invalid sender": {Sender: `"a b"@shared.test`, From: `"a b"@shared.test`, SPF: true, DKIM: []string{"shared.test"}},
	} {
		if _, ok := a.Identity(); ok {
			t.Fatal("counted without", name)
		}
		if made := rejects(t, h.fresh(t), a, nil, t0, 10); len(made) != 0 {
			t.Fatal("blocked without", name)
		}
	}
}

func TestAutomaticBlocksEscalate(t *testing.T) { eachHome(t, testEscalate) }

func testEscalate(t *testing.T, e *home) {
	a := authed("alice@spam.test")
	if made := rejects(t, e, a, nil, t0, 4); len(made) != 0 {
		t.Fatal("blocked below threshold")
	}
	// Five within an hour, not five in all: the first four age out.
	if made := rejects(t, e, a, nil, t0.Add(time.Hour+3*time.Minute), 1); len(made) != 0 {
		t.Fatal("window ignored")
	}
	at := t0.Add(2 * time.Hour)
	for level, cooldown := range []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour} {
		made := rejects(t, e, a, nil, at, 5)
		last := at.Add(4 * time.Minute)
		want := min(level+1, 3)
		if len(made) != 1 || made[0].Kind != "address" || made[0].Value != "alice@spam.test" || made[0].Source != "automatic" || made[0].Level != want || *made[0].Until != last.Add(cooldown).UnixMilli() {
			t.Fatal("wrong escalation", level, made)
		}
		if !blockedAt(t, e, "Alice@spam.test", last.Add(cooldown-time.Second)) || blockedAt(t, e, "alice@spam.test", last.Add(cooldown)) {
			t.Fatal("block does not expire at until", level)
		}
		at = last.Add(cooldown)
	}
	// A clean period without evidence resets the level.
	at = at.Add(30 * 24 * time.Hour)
	if made := rejects(t, e, a, nil, at, 5); len(made) != 1 || made[0].Level != 1 || *made[0].Until != at.Add(4*time.Minute+time.Hour).UnixMilli() {
		t.Fatal("clean period kept the level", made)
	}
	// Just short of it keeps the level.
	at = at.Add(4*time.Minute + 30*24*time.Hour - time.Minute)
	if made := rejects(t, e, a, nil, at, 5); len(made) != 1 || made[0].Level != 2 {
		t.Fatal("level reset early", made)
	}
	// The evidence holds IDs, counts and times only.
	raw := e.evidenceRaw(t)
	if len(raw) == 0 || bytes.Contains(raw, []byte("spam.test")) {
		t.Fatal("unexpected evidence content", string(raw))
	}
	var doc map[string]any
	if e.files() && (ParseEvidence(raw) != nil || json.Unmarshal(raw, &doc) != nil || len(doc) != 8) {
		t.Fatal("unexpected evidence shape", string(raw))
	}
}

func TestAutomaticBlocksExclusions(t *testing.T) { eachHome(t, testExclusions) }

func testExclusions(t *testing.T, e *home) {
	ctx := context.Background()
	// Own domains never count; the null sender has no identity.
	if made := rejects(t, e, authed("boss@example.test"), []string{"example.test"}, t0, 10); len(made) != 0 {
		t.Fatal("own domain blocked")
	}
	if e.wroteEvidence(t) {
		t.Fatal("own domain counted")
	}
	// A manual block is never replaced by an automatic one: neither on the
	// address nor when its domain is manually blocked.
	blocks := e
	if _, err := blocks.Put(ctx, SenderBlock{Kind: "address", Value: "mallory@spam.test", Source: "manual", Actor: "admin", Reason: "spam"}, nil, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := blocks.Put(ctx, SenderBlock{Kind: "domain", Value: "manual.test", Source: "manual", Actor: "admin", Reason: "spam"}, nil, t0); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"mallory@spam.test", "eve@manual.test"} {
		if made := rejects(t, e, authed(address), nil, t0, 5); len(made) != 0 {
			t.Fatal("automatic block over manual", address, made)
		}
	}
	list, err := blocks.List(ctx, t0)
	if err != nil || len(list) != 2 || list[0].Source != "manual" || list[1].Source != "manual" || list[1].Until != nil {
		t.Fatal("manual block changed", list, err)
	}
	// The level climbs only when a block was made: once a brief manual block
	// lapses, the first automatic block is level 1.
	soon := t0.Add(2 * time.Hour).UnixMilli()
	if _, err := blocks.Put(ctx, SenderBlock{Kind: "address", Value: "brief@spam.test", Until: &soon, Source: "manual", Actor: "admin", Reason: "spam"}, nil, t0); err != nil {
		t.Fatal(err)
	}
	if made := rejects(t, e, authed("brief@spam.test"), nil, t0, 10); len(made) != 0 {
		t.Fatal("automatic block over manual", made)
	}
	if made := rejects(t, e, authed("brief@spam.test"), nil, t0.Add(3*time.Hour), 5); len(made) != 1 || made[0].Level != 1 {
		t.Fatal("level climbed without a block", made)
	}
	// A manual unblock suppresses automatic re-blocking for UnblockWindow
	// and resets the escalation.
	a := authed("bob@spam.test")
	made := rejects(t, e, a, nil, t0, 5)
	if len(made) != 1 {
		t.Fatal("not blocked", made)
	}
	at := t0.Add(10 * time.Minute)
	if removed, err := blocks.Remove(ctx, made[0].ID, at); err != nil || !removed {
		t.Fatal(removed, err)
	}
	for _, start := range []time.Time{at, at.Add(30*24*time.Hour - time.Hour)} {
		if again := rejects(t, e, a, nil, start, 10); len(again) != 0 {
			t.Fatal("re-blocked inside the unblock window", start, again)
		}
	}
	at = at.Add(30 * 24 * time.Hour)
	if again := rejects(t, e, a, nil, at, 5); len(again) != 1 || again[0].Level != 1 {
		t.Fatal("unblock window never ended or kept the level", again)
	}
}

// warm starts the accepted-domain record long enough ago that automatic
// domain blocks are enabled at t0.
func warm(t *testing.T, s SenderStore) {
	t.Helper()
	if err := s.Accepted(context.Background(), authed("x@warm.test"), t0.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// blockAddresses blocks n authenticated addresses u<from>.. on domain, gap
// apart, and returns the domain blocks made.
func blockAddresses(t *testing.T, s SenderStore, domain string, from, n int, start time.Time, gap time.Duration) []SenderBlock {
	t.Helper()
	var domains []SenderBlock
	for i := from; i < from+n; i++ {
		for _, b := range rejects(t, s, authed(fmt.Sprintf("u%d@%s", i, domain)), nil, start.Add(time.Duration(i-from)*gap), 5) {
			if b.Kind == "domain" {
				domains = append(domains, b)
			}
		}
	}
	return domains
}

// Five distinct automatically blocked addresses within a day block a domain
// no authenticated mail was accepted from, once the record has warmed up.
func TestAutomaticDomainBlocks(t *testing.T) { eachHome(t, testDomainBlocks) }

func testDomainBlocks(t *testing.T, h *home) {
	ctx := context.Background()
	dir := h.fresh(t)
	warm(t, dir)
	if got := blockAddresses(t, dir, "throwaway.test", 0, 4, t0, time.Minute); len(got) != 0 {
		t.Fatal("domain blocked by four addresses", got)
	}
	got := blockAddresses(t, dir, "throwaway.test", 4, 1, t0.Add(10*time.Minute), time.Minute)
	if len(got) != 1 || got[0].Value != "throwaway.test" || got[0].Level != 1 || got[0].Source != "automatic" {
		t.Fatal("domain not blocked", got)
	}
	at := t0.Add(20 * time.Minute)
	if !blockedAt(t, dir, "new@throwaway.test", at) || blockedAt(t, dir, "new@sub.throwaway.test", at) {
		t.Fatal("domain block does not match exactly")
	}
	// After it expires, the addresses that made it do not make it again:
	// five new ones within a day do, one level up.
	for i, b := range blockAddresses(t, dir, "throwaway.test", 5, 5, t0.Add(2*time.Hour), time.Minute) {
		if i > 0 || b.Level != 2 {
			t.Fatal("domain re-blocked by old evidence", b)
		}
	}
	if !blockedAt(t, dir, "new@throwaway.test", t0.Add(3*time.Hour)) {
		t.Fatal("domain not re-blocked")
	}
	// Spread over more than a day: never five at once.
	dir = h.fresh(t)
	warm(t, dir)
	if got := blockAddresses(t, dir, "slow.test", 0, 5, t0, 7*time.Hour); len(got) != 0 {
		t.Fatal("window ignored", got)
	}
	// Warm-up: no domain blocks until authenticated acceptances have been
	// recorded for 30 days, and none before any.
	dir = h.fresh(t)
	if got := blockAddresses(t, dir, "early.test", 0, 5, t0, time.Minute); len(got) != 0 {
		t.Fatal("domain blocked with no accepted-domain record", got)
	}
	if err := dir.Accepted(ctx, authed("x@warm.test"), t0.Add(-29*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := blockAddresses(t, dir, "early.test", 5, 5, t0.Add(2*time.Hour), time.Minute); len(got) != 0 {
		t.Fatal("domain blocked during warm-up", got)
	}
	if got := blockAddresses(t, dir, "early.test", 10, 5, t0.Add(25*24*time.Hour), time.Minute); len(got) != 1 {
		t.Fatal("domain not blocked after warm-up", got)
	}
	// Authenticated mail accepted from the domain, even long ago, rules it
	// out; a forged envelope domain on accepted mail does not.
	dir = h.fresh(t)
	warm(t, dir)
	if err := dir.Accepted(ctx, Authentication{Sender: "Someone@Provider.TEST", From: "someone@provider.test", SPF: true, DKIM: []string{"provider.test"}}, t0.Add(-365*24*time.Hour+time.Hour)); err != nil {
		t.Fatal(err)
	}
	spoofed := Authentication{Sender: "x@forged.test", From: "x@forged.test", DKIM: []string{"forged.test"}}
	if err := dir.Accepted(ctx, spoofed, t0); err != nil {
		t.Fatal(err)
	}
	if got := blockAddresses(t, dir, "provider.test", 0, 6, t0, time.Minute); len(got) != 0 {
		t.Fatal("shared provider blocked", got)
	}
	if !blockedAt(t, dir, "u0@provider.test", t0.Add(time.Minute)) {
		t.Fatal("address blocks still apply on a good domain")
	}
	if got := blockAddresses(t, dir, "forged.test", 0, 5, t0, time.Minute); len(got) != 1 {
		t.Fatal("unauthenticated acceptance protected a domain", got)
	}
	// Recorded domains are never evicted: a full record stops recording,
	// says so, and keeps domain blocks for unrecorded domains only.
	dir = h.fresh(t)
	good := map[string]int64{BlockID("domain", "real.test"): t0.Add(-40 * 24 * time.Hour).UnixMilli()}
	for i := 1; i < MaxGoodDomains-1; i++ {
		good[BlockID("domain", fmt.Sprintf("d%05d.test", i))] = t0.Add(-40 * 24 * time.Hour).UnixMilli()
	}
	dir.seedEvidence(t, evidenceDoc{Version: 1, Good: good, GoodSince: t0.Add(-40 * 24 * time.Hour).UnixMilli()})
	if status := dir.Status(ctx, t0); status.GoodFull {
		t.Fatal("not yet full", status)
	}
	for i := range 20 { // a flood of authenticated throwaway domains
		if err := dir.Accepted(ctx, authed(fmt.Sprintf("x@flood%d.test", i)), t0); err != nil {
			t.Fatal(err)
		}
	}
	doc := dir.evidence(t)
	if len(doc.Good) != MaxGoodDomains || doc.Good[BlockID("domain", "real.test")] == 0 || doc.Good[BlockID("domain", "flood1.test")] != 0 {
		t.Fatal("flood changed the record", len(doc.Good))
	}
	if status := dir.Status(ctx, t0); !status.GoodFull {
		t.Fatal("full record not reported", status)
	}
	if got := blockAddresses(t, dir, "real.test", 0, 5, t0, time.Minute); len(got) != 0 {
		t.Fatal("a recorded domain lost its protection", got)
	}
	if got := blockAddresses(t, dir, "flood1.test", 0, 5, t0, time.Minute); len(got) != 1 {
		t.Fatal("a full record disabled domain blocks", got)
	}
	// One crude parent (last two labels) records at most 50 domains.
	dir = h.fresh(t)
	warm(t, dir)
	for i := range 60 {
		if err := dir.Accepted(ctx, authed(fmt.Sprintf("x@s%d.wild.test", i)), t0.Add(-31*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	doc = dir.evidence(t)
	if doc.Good[BlockID("domain", "s49.wild.test")] == 0 || doc.Good[BlockID("domain", "s50.wild.test")] != 0 || doc.Parents[BlockID("domain", "wild.test")] != 50 {
		t.Fatal("per-parent limit", len(doc.Good))
	}
	if err := dir.Accepted(ctx, authed("x@other.test"), t0); err != nil {
		t.Fatal(err)
	}
	if doc = dir.evidence(t); doc.Good[BlockID("domain", "other.test")] == 0 {
		t.Fatal("another parent refused")
	}
	if got := blockAddresses(t, dir, "s55.wild.test", 0, 5, t0, time.Minute); len(got) != 1 {
		t.Fatal("unrecorded subdomain protected", got)
	}
	// An unblocked domain is suppressed like an address.
	dir = h.fresh(t)
	warm(t, dir)
	got = blockAddresses(t, dir, "again.test", 0, 5, t0, time.Minute)
	if len(got) != 1 {
		t.Fatal("domain not blocked", got)
	}
	if removed, err := dir.Remove(ctx, got[0].ID, t0.Add(10*time.Minute)); err != nil || !removed {
		t.Fatal(err)
	}
	if got := blockAddresses(t, dir, "again.test", 5, 5, t0.Add(20*time.Minute), time.Minute); len(got) != 0 {
		t.Fatal("unblocked domain re-blocked", got)
	}
}

func TestAutomaticBlocksEvidenceBounded(t *testing.T) { eachHome(t, testEvidenceBounded) }

func testEvidenceBounded(t *testing.T, e *home) {
	// A known abuser at level 2, then a flood of one-off identities filling
	// the cap: the abuser's level survives, the oldest one-offs go.
	abuser := authed("abuser@spam.test")
	rejects(t, e, abuser, nil, t0, 5)
	rejects(t, e, abuser, nil, t0.Add(2*time.Hour), 5)
	addresses := map[string]evidenceRecord{}
	doc := e.evidence(t)
	maps.Copy(addresses, doc.Addresses)
	for i := len(addresses); i < MaxEvidence; i++ {
		addresses[BlockID("address", fmt.Sprintf("u%d@spam.test", i))] = evidenceRecord{Domain: BlockID("domain", "spam.test"), Hits: []int64{t0.UnixMilli() + int64(i)}, Last: t0.Add(3*time.Hour).UnixMilli() + int64(i)}
	}
	doc.Addresses = addresses
	e.seedEvidence(t, doc)
	for i := range 3 {
		rejects(t, e, authed(fmt.Sprintf("new%d@spam.test", i)), nil, t0.Add(4*time.Hour), 1)
	}
	if doc = e.evidence(t); len(doc.Addresses) != MaxEvidence || doc.Addresses[BlockID("address", "abuser@spam.test")].Level != 2 {
		t.Fatal("eviction reset a known abuser", len(doc.Addresses))
	}
	if _, ok := doc.Addresses[BlockID("address", "new2@spam.test")]; !ok {
		t.Fatal("newest record evicted")
	}
	if made := rejects(t, e, abuser, nil, t0.Add(30*time.Hour), 5); len(made) != 1 || made[0].Level != 3 {
		t.Fatal("abuser escalation lost", made)
	}
	if !e.files() {
		return
	}
	over := evidenceDoc{Version: 1, Addresses: map[string]evidenceRecord{}}
	for i := range MaxEvidence + 1 {
		over.Addresses[fmt.Sprintf("%016x", i)] = evidenceRecord{Domain: fmt.Sprintf("%016x", i), Last: 1}
	}
	overRaw, _ := json.Marshal(over)
	id := BlockID("address", "a@b.test")
	for name, bad := range map[string]string{
		"over cap":      string(overRaw),
		"unknown":       `{"version":1,"subject":"hello"}`,
		"version":       `{"version":2}`,
		"raw address":   `{"version":1,"addresses":{"a@b.test":{"domain":"` + id + `","hits":[],"level":1,"last":5,"blocked":0}}}`,
		"no domain":     `{"version":1,"addresses":{"` + id + `":{"hits":[],"level":1,"last":5,"blocked":0}}}`,
		"hits":          `{"version":1,"addresses":{"` + id + `":{"domain":"` + id + `","hits":[1,2,3,4,5],"level":0,"last":5,"blocked":0}}}`,
		"level":         `{"version":1,"addresses":{"` + id + `":{"domain":"` + id + `","hits":[],"level":4,"last":5,"blocked":0}}}`,
		"domain domain": `{"version":1,"domains":{"` + id + `":{"domain":"` + id + `","hits":[],"level":1,"last":5,"blocked":5}}}`,
		"good":          `{"version":1,"good":{"b.test":5}}`,
		"unblock id":    `{"version":1,"unblocked":{"alice@b.test":5}}`,
		"trailing":      `{"version":1} {}`,
		"oversized":     `{"version":1}` + strings.Repeat(" ", MaxEvidenceBytes),
	} {
		if ParseEvidence([]byte(bad)) == nil {
			t.Fatal("accepted", name)
		}
	}
	// Like Blocks, Evidence never creates the receiving directory.
	if _, err := NewEvidence(filepath.Join(e.dir, "missing")).Reject(context.Background(), authed("x@spam.test"), nil, t0); !errors.Is(err, ErrBlockStore) {
		t.Fatal("created the receiving directory", err)
	}
}

// Every map at its cap with the widest values stays well under the byte
// bound: the file is bounded by construction, not by luck. The tables hold the
// same records, and a write at every cap keeps every cap and the same bound.
func TestEvidenceWorstCaseSize(t *testing.T) { eachHome(t, testEvidenceWorstCaseSize) }

func testEvidenceWorstCaseSize(t *testing.T, h *home) {
	widest := int64(1<<53 - 1)
	hits := []int64{widest, widest, widest, widest}
	doc := evidenceDoc{Version: 1, Addresses: map[string]evidenceRecord{}, Domains: map[string]evidenceRecord{}, Good: map[string]int64{}, Parents: map[string]int{}, Unblocked: map[string]int64{}, GoodSince: widest, ResetAt: widest}
	for i := range MaxGoodDomains {
		id := fmt.Sprintf("%016x", uint64(i)<<40)
		if i < MaxEvidence {
			doc.Addresses[id] = evidenceRecord{Domain: id, Hits: hits, Level: 3, Last: widest, Blocked: widest}
			doc.Domains[id] = evidenceRecord{Hits: hits, Level: 3, Last: widest, Blocked: widest}
			doc.Unblocked[id] = widest
		}
		doc.Good[id] = widest
		doc.Parents[id] = maxGoodPerParent
	}
	if h.files() {
		raw, err := json.Marshal(doc)
		if err != nil || len(raw) > MaxEvidenceBytes*3/4 || ParseEvidence(raw) != nil {
			t.Fatal("worst case", len(raw), err)
		}
		return
	}
	h.seedEvidence(t, doc)
	ctx, now := context.Background(), time.UnixMilli(widest-1)
	if _, err := h.Reject(ctx, authed("new@x.test"), nil, now); err != nil {
		t.Fatal(err)
	}
	if err := h.Accepted(ctx, authed("new@y.test"), now); err != nil {
		t.Fatal(err)
	}
	if err := unblocked(ctx, h.SenderStore.(tableSenders), BlockID("address", "new@z.test"), now); err != nil {
		t.Fatal(err)
	}
	after := h.evidence(t)
	if len(after.Addresses) != MaxEvidence || len(after.Domains) != MaxEvidence || len(after.Unblocked) != MaxEvidence || len(after.Good) != MaxGoodDomains {
		t.Fatal("a cap moved", len(after.Addresses), len(after.Domains), len(after.Unblocked), len(after.Good))
	}
	// Stored bytes: every text column plus three 8-byte integers per row.
	var size int64
	if err := h.db.QueryRow(`SELECT sum(length(kind) + length(id) + length(ref) + length(hits) + 24) FROM sender_evidence`).Scan(&size); err != nil || size > MaxEvidenceBytes*3/4 {
		t.Fatal("worst case", size, err)
	}
}

// Damaged or oversized evidence never needs file surgery: unblocking works,
// counting restarts after setting the file aside, and status reports it.
func TestAutomaticBlocksEvidenceRecovery(t *testing.T) { eachHome(t, testEvidenceRecovery) }

func testEvidenceRecovery(t *testing.T, h *home) {
	ctx := context.Background()
	for name, poison := range map[string][]byte{"malformed": []byte("{"), "oversized": bytes.Repeat([]byte(" "), MaxEvidenceBytes+1)} {
		if !h.files() {
			break // no file to damage
		}
		dir := t.TempDir()
		e := FileSenders(dir)
		made := rejects(t, e, authed("bob@spam.test"), nil, t0, 5)
		if len(made) != 1 {
			t.Fatal(name, made)
		}
		path := filepath.Join(dir, EvidenceFile)
		if err := os.WriteFile(path, poison, 0o600); err != nil {
			t.Fatal(err)
		}
		if status := e.Status(ctx, t0); !status.Damaged {
			t.Fatal(name, "damage not reported", status)
		}
		if removed, err := NewBlocks(dir).Remove(ctx, made[0].ID, t0.Add(time.Minute)); err != nil || !removed {
			t.Fatal(name, "unblock failed", removed, err)
		}
		if aside, err := os.ReadFile(filepath.Join(dir, EvidenceDamagedFile)); err != nil || !bytes.Equal(aside, poison) {
			t.Fatal(name, "damaged file not set aside", err)
		}
		if status := e.Status(ctx, t0); status.Damaged || status.ResetAt == nil || *status.ResetAt != t0.Add(time.Minute).UnixMilli() {
			t.Fatal(name, "reset not reported", status)
		}
		// The unblock was recorded in the fresh file; others count again.
		if again := rejects(t, e, authed("bob@spam.test"), nil, t0.Add(time.Hour), 5); len(again) != 0 {
			t.Fatal(name, "suppression lost", again)
		}
		if again := rejects(t, e, authed("eve@spam.test"), nil, t0.Add(time.Hour), 5); len(again) != 1 {
			t.Fatal(name, "counting did not recover", again)
		}
	}
	// An evidence store that cannot be written never keeps a block in force.
	made := rejects(t, h, authed("bob@spam.test"), nil, t0, 5)
	if path := filepath.Join(h.dir, EvidenceFile); h.files() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		h.exec(t, `DROP TABLE sender_evidence`)
	}
	removed, err := h.Remove(ctx, made[0].ID, t0.Add(time.Minute))
	if !removed || !errors.Is(err, ErrUnblockNotRecorded) || blockedAt(t, h, "bob@spam.test", t0.Add(time.Minute)) {
		t.Fatal("unblock depended on evidence", removed, err)
	}
}

// Automatic blocks never cost an administrator a block.
func TestAutomaticBlocksBudget(t *testing.T) { eachHome(t, testBudget) }

func testBudget(t *testing.T, h *home) {
	ctx := context.Background()
	write := func(t *testing.T, dir *home, blocks []SenderBlock) {
		t.Helper()
		dir.seedBlocks(t, blocks)
	}
	auto := func(value string, until time.Duration) SenderBlock {
		u := t0.Add(until).UnixMilli()
		return SenderBlock{ID: BlockID("address", value), Kind: "address", Value: value, Until: &u, Source: "automatic", Level: 1, CreatedAt: t0.UnixMilli(), Actor: "automatic", Reason: "abuse"}
	}
	manual := func(value string) SenderBlock {
		return SenderBlock{ID: BlockID("address", value), Kind: "address", Value: value, Source: "manual", CreatedAt: t0.UnixMilli(), Actor: "admin", Reason: "spam"}
	}
	put := func(dir *home, b SenderBlock) error {
		_, err := dir.Put(ctx, b, nil, t0)
		return err
	}
	count := func(t *testing.T, dir *home) (manuals, autos int, ids map[string]bool) {
		t.Helper()
		list, err := dir.List(ctx, t0)
		if err != nil {
			t.Fatal(err)
		}
		ids = map[string]bool{}
		for _, b := range list {
			ids[b.ID] = true
			if b.Source == "manual" {
				manuals++
			} else {
				autos++
			}
		}
		return manuals, autos, ids
	}
	// Count: the list full, half automatic; a manual add evicts the
	// soonest-expiring automatic block.
	dir := h.fresh(t)
	var blocks []SenderBlock
	for i := range MaxBlocks / 2 {
		blocks = append(blocks, manual(fmt.Sprintf("m%d@x.test", i)), auto(fmt.Sprintf("a%d@x.test", i), time.Duration(i+2)*time.Hour))
	}
	write(t, dir, blocks)
	if err := put(dir, manual("admin@x.test")); err != nil {
		t.Fatal("manual add refused by automatic blocks", err)
	}
	if m, a, ids := count(t, dir); m != MaxBlocks/2+1 || a != MaxBlocks/2-1 || ids[BlockID("address", "a0@x.test")] || !ids[BlockID("address", "a1@x.test")] {
		t.Fatal("wrong eviction", m, a)
	}
	// The automatic share full: a new automatic block is refused and every
	// existing block stays; an automatic block never evicts.
	dir2 := h.fresh(t)
	blocks = nil
	for i := range MaxBlocks / 2 {
		blocks = append(blocks, auto(fmt.Sprintf("a%d@x.test", i), time.Duration(i+2)*time.Hour))
	}
	write(t, dir2, blocks)
	before := dir2.blocksRaw(t)
	if status := dir2.Status(ctx, t0); !status.AutomaticFull {
		t.Fatal("full automatic share not reported", status)
	}
	if err := put(dir2, auto("extra@x.test", time.Hour)); !errors.Is(err, ErrAutomaticFull) {
		t.Fatal("automatic block past its share", err)
	}
	if after := dir2.blocksRaw(t); !bytes.Equal(after, before) {
		t.Fatal("automatic block evicted another")
	}
	// Through Reject: refused, surfaced, no escalation, no change.
	var made []SenderBlock
	var err error
	for i := range 5 {
		made, err = dir2.Reject(ctx, authed("extra@x.test"), nil, t0.Add(time.Duration(i)*time.Minute))
	}
	if len(made) != 0 || !errors.Is(err, ErrAutomaticFull) {
		t.Fatal("refusal not surfaced", made, err)
	}
	if after := dir2.blocksRaw(t); !bytes.Equal(after, before) {
		t.Fatal("refused candidate changed the list")
	}
	if doc := dir2.evidence(t); doc.Addresses[BlockID("address", "extra@x.test")].Level != 0 {
		t.Fatal("refused candidate escalated")
	}
	// A manual add within the total evicts nothing.
	if err := put(dir2, manual("admin@x.test")); err != nil {
		t.Fatal(err)
	}
	if _, a, ids := count(t, dir2); a != MaxBlocks/2 || !ids[BlockID("address", "a0@x.test")] {
		t.Fatal("manual add within the total evicted", a)
	}
	// One worst-case block fits the headroom automaticFull assumes.
	worst := strings.Repeat("&", 316) + "@a.b"
	if n := cfreceiving.BlockWireBytes(cfreceiving.Block{Address: worst}); n > maxOneBlockWire {
		t.Fatal("one block exceeds maxOneBlockWire", n)
	}
	// Only manual blocks fill the list: a manual add is refused, an
	// automatic one too.
	blocks = nil
	for i := range MaxBlocks {
		blocks = append(blocks, manual(fmt.Sprintf("m%d@x.test", i)))
	}
	write(t, dir, blocks)
	if err := put(dir, manual("admin@x.test")); !errors.Is(err, ErrBlockFull) {
		t.Fatal(err)
	}
	if err := put(dir, auto("new@x.test", time.Hour)); !errors.Is(err, ErrAutomaticFull) {
		t.Fatal(err)
	}
	// Wire budget: long automatic values hold the automatic half of it.
	long := func(i int) string { return fmt.Sprintf("%0200d@%s.test", i, strings.Repeat("d", 40)) }
	blocks = nil
	for i := range 1700 { // about 500 KiB, under the whole wire budget
		blocks = append(blocks, auto(long(i), time.Duration(i+2)*time.Hour))
	}
	write(t, dir, blocks)
	if err := put(dir, auto(long(9999), time.Hour)); !errors.Is(err, ErrAutomaticFull) {
		t.Fatal("automatic wire share", err)
	}
	if err := put(dir, manual(long(9998))); err != nil {
		t.Fatal("manual add refused by automatic wire use", err)
	}
	wire, n := 0, 0
	list, _ := dir.List(ctx, t0)
	for _, x := range list {
		if x.Source == "automatic" {
			wire, n = wire+cfreceiving.BlockWireBytes(x.Wire()), n+1
		}
	}
	if wire > maxBlockWire/2 || wire < maxBlockWire/2-2*maxOneBlockWire {
		t.Fatal("manual add left automatic blocks over their share", wire, n)
	}
	// Within one worst-case block of the wire share counts as full.
	blocks, wire = nil, 0
	for i := 0; wire <= maxBlockWire/2-maxOneBlockWire; i++ {
		b := auto(long(i), time.Duration(i+2)*time.Hour)
		blocks, wire = append(blocks, b), wire+cfreceiving.BlockWireBytes(b.Wire())
	}
	write(t, dir, blocks)
	if status := dir.Status(ctx, t0); !status.AutomaticFull || len(blocks) >= MaxBlocks/2 || wire > maxBlockWire/2 {
		t.Fatal("wire-full automatic share not reported", len(blocks), wire)
	}
}
