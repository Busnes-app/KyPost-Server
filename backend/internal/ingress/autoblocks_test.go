package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func authed(address string) Authentication {
	return Authentication{Sender: address, From: address, SPF: true, DKIM: []string{address[strings.LastIndexByte(address, '@')+1:]}}
}

// rejects counts n reject verdicts one minute apart from start and returns
// the blocks made.
func rejects(t *testing.T, e Evidence, a Authentication, own []string, start time.Time, n int) []SenderBlock {
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

func blockedAt(t *testing.T, dir, sender string, at time.Time) bool {
	t.Helper()
	blocked, err := NewBlocks(dir).Blocked(sender, at)
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

// The design's offline qualification: five reject verdicts that pass SPF and
// DKIM for a shared domain, but whose envelope sender and From differ, block
// nobody, however often they repeat.
func TestAutomaticBlocksSharedDomainSpoofing(t *testing.T) {
	dir := t.TempDir()
	e := NewEvidence(dir)
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
			if blockedAt(t, dir, sender, t0.Add(time.Minute)) {
				t.Fatal("blocked", sender)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, EvidenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncounted verdicts wrote evidence", err)
	}
}

func TestAutomaticBlocksEvidenceConditions(t *testing.T) {
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
		dir := t.TempDir()
		if made := rejects(t, NewEvidence(dir), a, nil, t0, 10); len(made) != 0 {
			t.Fatal("blocked without", name)
		}
	}
}

func TestAutomaticBlocksEscalate(t *testing.T) {
	dir := t.TempDir()
	e := NewEvidence(dir)
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
		if !blockedAt(t, dir, "Alice@spam.test", last.Add(cooldown-time.Second)) || blockedAt(t, dir, "alice@spam.test", last.Add(cooldown)) {
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
	// The evidence holds addresses, counts and times only.
	raw, err := os.ReadFile(filepath.Join(dir, EvidenceFile))
	if err != nil || ParseEvidence(raw) != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || len(doc) != 5 {
		t.Fatal("unexpected evidence shape", string(raw))
	}
}

func TestAutomaticBlocksExclusions(t *testing.T) {
	dir := t.TempDir()
	e := NewEvidence(dir)
	ctx := context.Background()
	// Own domains never count; the null sender has no identity.
	if made := rejects(t, e, authed("boss@example.test"), []string{"example.test"}, t0, 10); len(made) != 0 {
		t.Fatal("own domain blocked")
	}
	if _, err := os.Stat(filepath.Join(dir, EvidenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own domain counted", err)
	}
	// A manual block is never replaced by an automatic one: neither on the
	// address nor when its domain is manually blocked.
	blocks := NewBlocks(dir)
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
	list, err := blocks.List(t0)
	if err != nil || len(list) != 2 || list[0].Source != "manual" || list[1].Source != "manual" || list[1].Until != nil {
		t.Fatal("manual block changed", list, err)
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

// Five distinct automatically blocked addresses within a day block a domain
// mail was never accepted from; a domain with accepted mail is never blocked.
func TestAutomaticDomainBlocks(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, dir, domain string, n int, gap time.Duration) []SenderBlock {
		t.Helper()
		var domains []SenderBlock
		for i := range n {
			for _, b := range rejects(t, NewEvidence(dir), authed(fmt.Sprintf("u%d@%s", i, domain)), nil, t0.Add(time.Duration(i)*gap), 5) {
				if b.Kind == "domain" {
					domains = append(domains, b)
				}
			}
		}
		return domains
	}
	if got := run(t, t.TempDir(), "throwaway.test", 4, time.Minute); len(got) != 0 {
		t.Fatal("domain blocked by four addresses", got)
	}
	dir := t.TempDir()
	if got := run(t, dir, "throwaway.test", 5, time.Minute); len(got) != 1 || got[0].Value != "throwaway.test" || got[0].Level != 1 || got[0].Source != "automatic" {
		t.Fatal("domain not blocked", got)
	} else if !blockedAt(t, dir, "new@throwaway.test", t0.Add(time.Hour)) || blockedAt(t, dir, "new@sub.throwaway.test", t0.Add(time.Hour)) {
		t.Fatal("domain block does not match exactly")
	}
	// After it expires, the addresses that made it do not make it again:
	// five new ones within a day do, one level up.
	after := t0.Add(2 * time.Hour)
	for i := 5; i < 10; i++ {
		for _, b := range rejects(t, NewEvidence(dir), authed(fmt.Sprintf("u%d@throwaway.test", i)), nil, after.Add(time.Duration(i)*time.Minute), 5) {
			if b.Kind == "domain" && (i < 9 || b.Level != 2) {
				t.Fatal("domain re-blocked by old evidence", i, b)
			}
		}
	}
	if !blockedAt(t, dir, "new@throwaway.test", after.Add(time.Hour)) {
		t.Fatal("domain not re-blocked")
	}
	// Spread over more than a day: never five at once.
	if got := run(t, t.TempDir(), "slow.test", 5, 7*time.Hour); len(got) != 0 {
		t.Fatal("window ignored", got)
	}
	// Any accepted mail from the domain, even long ago, rules it out.
	dir = t.TempDir()
	if err := NewEvidence(dir).Accepted(ctx, "Someone@Provider.TEST", t0.Add(-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "provider.test", 6, time.Minute); len(got) != 0 {
		t.Fatal("shared provider blocked", got)
	}
	if blockedAt(t, dir, "u0@provider.test", t0.Add(time.Minute)) == false {
		t.Fatal("address blocks still apply on a good domain")
	}
	// Null and malformed senders record nothing.
	for _, sender := range []string{"", "<>", "x@[1.2.3.4]"} {
		if err := NewEvidence(dir).Accepted(ctx, sender, t0); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, EvidenceFile))
	if !strings.Contains(string(raw), `"good":["provider.test"]`) {
		t.Fatal("good domains", string(raw))
	}
	// A full accepted-domain record can no longer prove a domain never-good,
	// so it disables automatic domain blocks instead of forgetting one.
	dir = t.TempDir()
	good := make([]string, MaxGoodDomains)
	for i := range good {
		good[i] = fmt.Sprintf("d%05d.test", i)
	}
	full, _ := json.Marshal(map[string]any{"version": 1, "good": good})
	if err := os.WriteFile(filepath.Join(dir, EvidenceFile), full, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewEvidence(dir).Accepted(ctx, "x@new.test", t0); !errors.Is(err, ErrGoodDomainsFull) {
		t.Fatal("full record grew", err)
	}
	if err := NewEvidence(dir).Accepted(ctx, "x@d00001.test", t0); err != nil {
		t.Fatal("known domain refused", err)
	}
	if got := run(t, dir, "throwaway.test", 5, time.Minute); len(got) != 0 {
		t.Fatal("domain blocked with a full record", got)
	}
	// An unblocked domain is suppressed like an address.
	dir = t.TempDir()
	got := run(t, dir, "again.test", 5, time.Minute)
	if len(got) != 1 {
		t.Fatal("domain not blocked", got)
	}
	if removed, err := NewBlocks(dir).Remove(ctx, got[0].ID, t0.Add(10*time.Minute)); err != nil || !removed {
		t.Fatal(err)
	}
	for i := 5; i < 10; i++ {
		for _, b := range rejects(t, NewEvidence(dir), authed(fmt.Sprintf("u%d@again.test", i)), nil, t0.Add(20*time.Minute), 5) {
			if b.Kind == "domain" {
				t.Fatal("unblocked domain re-blocked", b)
			}
		}
	}
}

func TestAutomaticBlocksEvidenceBounded(t *testing.T) {
	dir := t.TempDir()
	addresses := map[string]evidenceRecord{}
	for i := range MaxEvidence {
		addresses[fmt.Sprintf("u%d@spam.test", i)] = evidenceRecord{Hits: []int64{t0.UnixMilli() + int64(i)}, Last: t0.UnixMilli() + int64(i)}
	}
	raw, _ := json.Marshal(evidenceDoc{Version: 1, Addresses: addresses})
	if err := os.WriteFile(filepath.Join(dir, EvidenceFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rejects(t, NewEvidence(dir), authed("new@spam.test"), nil, t0.Add(time.Hour), 1)
	doc, err := NewEvidence(dir).load()
	if err != nil || len(doc.Addresses) != MaxEvidence {
		t.Fatal("evidence unbounded", len(doc.Addresses), err)
	}
	if _, ok := doc.Addresses["u0@spam.test"]; ok {
		t.Fatal("oldest record kept")
	}
	if _, ok := doc.Addresses["new@spam.test"]; !ok {
		t.Fatal("newest record evicted")
	}
	addresses["over@spam.test"] = evidenceRecord{Last: 1}
	over, _ := json.Marshal(evidenceDoc{Version: 1, Addresses: addresses})
	for name, bad := range map[string]string{
		"over cap":     string(over),
		"unknown":      `{"version":1,"subject":"hello"}`,
		"version":      `{"version":2}`,
		"hits":         `{"version":1,"addresses":{"a@b.test":{"hits":[1,2,3,4,5],"level":0,"last":5,"blocked":0}}}`,
		"level":        `{"version":1,"addresses":{"a@b.test":{"hits":[],"level":4,"last":5,"blocked":0}}}`,
		"address":      `{"version":1,"addresses":{"A@b.test":{"hits":[],"level":1,"last":5,"blocked":0}}}`,
		"unsorted":     `{"version":1,"good":["b.test","a.test"]}`,
		"duplicate":    `{"version":1,"good":["a.test","a.test"]}`,
		"unblock id":   `{"version":1,"unblocked":{"alice@b.test":5}}`,
		"trailing":     `{"version":1} {}`,
		"domain value": `{"version":1,"domains":{"a@b.test":{"hits":[],"level":1,"last":5,"blocked":5}}}`,
	} {
		if ParseEvidence([]byte(bad)) == nil {
			t.Fatal("accepted", name)
		}
	}
	// A malformed file refuses counting rather than starting over.
	if err := os.WriteFile(filepath.Join(dir, EvidenceFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEvidence(dir).Reject(context.Background(), authed("x@spam.test"), nil, t0); err == nil {
		t.Fatal("malformed evidence replaced")
	}
	// Like Blocks, Evidence never creates the receiving directory.
	if _, err := NewEvidence(filepath.Join(dir, "missing")).Reject(context.Background(), authed("x@spam.test"), nil, t0); !errors.Is(err, ErrBlockStore) {
		t.Fatal("created the receiving directory", err)
	}
}
