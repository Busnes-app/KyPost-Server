package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
)

func TestSenderBlocks(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1790000000000)
	s := NewBlocks(t.TempDir())
	own := []string{"example.test"}
	manual := func(kind, value string) SenderBlock {
		return SenderBlock{Kind: kind, Value: value, Source: "manual", Actor: "admin-1", Reason: "spam"}
	}

	// Normalisation is the shared fixture's (TestSenderBlocksMatchWorker);
	// a few refusals beside it.
	for _, in := range [][2]string{{"domain", "\u212aevil.test"}, {"address", "a@bücher.test"}, {"domain", "a b.test"}, {"address", strings.Repeat("a", 314) + "@x.test"}, {"domain", ""}} {
		if _, err := s.Put(ctx, manual(in[0], in[1]), own, now); !errors.Is(err, ErrBlockInvalid) {
			t.Fatal("invalid block accepted", in, err)
		}
	}
	// Own domains, and addresses on them, cannot be blocked.
	for _, in := range [][2]string{{"domain", "Example.test"}, {"address", "someone@example.test"}} {
		if _, err := s.Put(ctx, manual(in[0], in[1]), own, now); !errors.Is(err, ErrBlockOwn) {
			t.Fatal("own domain blocked", in, err)
		}
	}
	// Free text is not a reason; past until is refused.
	bad := manual("domain", "evil.test")
	bad.Reason = "they wrote: hello"
	past := now.UnixMilli()
	if _, err := s.Put(ctx, bad, own, now); !errors.Is(err, ErrBlockInvalid) {
		t.Fatal("free-text reason accepted", err)
	}
	bad.Reason, bad.Until = "spam", &past
	if _, err := s.Put(ctx, bad, own, now); !errors.Is(err, ErrBlockInvalid) {
		t.Fatal("expired block accepted", err)
	}

	// CRUD.
	if _, err := s.Put(ctx, manual("address", "Bad@Spam.test"), own, now); err != nil {
		t.Fatal(err)
	}
	soon := now.Add(time.Hour).UnixMilli()
	timed := manual("domain", "evil.test")
	timed.Until = &soon
	if b, err := s.Put(ctx, timed, own, now); err != nil || b.ID == "" || b.CreatedAt != now.UnixMilli() {
		t.Fatal(b, err)
	}
	list, err := s.List(now)
	if err != nil || len(list) != 2 || list[0].Value != "bad@spam.test" || list[1].Value != "evil.test" || list[0].Source != "manual" || list[0].Actor != "admin-1" {
		t.Fatalf("list %+v %v", list, err)
	}
	for sender, want := range map[string]bool{
		"bad@spam.test": true, "BAD@SPAM.TEST": true, "other@spam.test": false, "x@evil.test": true, "x@EVIL.test": true,
		"x@sub.evil.test": false, "x@notevil.test": false, "": false, "postmaster": false,
	} {
		if got, err := s.Blocked(sender, now); err != nil || got != want {
			t.Fatal("blocked", sender, got, err)
		}
	}
	// Expired blocks are ignored, and pruned at the next write.
	later := now.Add(2 * time.Hour)
	if got, _ := s.Blocked("x@evil.test", later); got {
		t.Fatal("expired block enforced")
	}
	for _, id := range []string{"", "BAD", BlockID("address", "bad@spam.test")[:15], strings.ToUpper(BlockID("address", "bad@spam.test"))} {
		if _, err := s.Remove(ctx, id, later); !errors.Is(err, ErrBlockInvalid) {
			t.Fatal("malformed id accepted", id, err)
		}
	}
	if found, err := s.Remove(ctx, BlockID("address", "bad@spam.test"), later); err != nil || !found {
		t.Fatal("remove", found, err)
	}
	if found, err := s.Remove(ctx, BlockID("address", "bad@spam.test"), later); err != nil || found {
		t.Fatal("second remove", found, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.dir, BlocksFile))
	if strings.Contains(string(raw), "evil.test") || strings.Contains(string(raw), "bad@spam.test") {
		t.Fatal("expired or removed block kept", string(raw))
	}
	if info, err := os.Stat(filepath.Join(s.dir, BlocksFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("mode", err)
	}

	// The automatic seam: source automatic needs an escalation level.
	auto := SenderBlock{Kind: "address", Value: "auto@spam.test", Source: "automatic", Actor: "maddy-local", Reason: "abuse", Until: &soon}
	if _, err := s.Put(ctx, auto, own, now); !errors.Is(err, ErrBlockInvalid) {
		t.Fatal("automatic block without level", err)
	}
	auto.Level = 1
	if _, err := s.Put(ctx, auto, own, now); err != nil {
		t.Fatal(err)
	}

	// Cap: MaxBlocks entries, concurrent writers lose nothing.
	full := NewBlocks(t.TempDir())
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 10 {
				if _, err := full.Put(ctx, manual("domain", "w"+strconv.Itoa(w)+"-"+strconv.Itoa(i)+".test"), own, now); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if list, err := full.List(now); err != nil || len(list) != 40 {
		t.Fatal("concurrent writers lost blocks", len(list), err)
	}
	blocks, _ := full.load()
	for i := len(blocks); i < MaxBlocks; i++ {
		blocks = append(blocks, SenderBlock{ID: BlockID("domain", "d"+strconv.Itoa(i)+".test"), Kind: "domain", Value: "d" + strconv.Itoa(i) + ".test", Source: "manual", CreatedAt: 1, Actor: "a", Reason: "spam"})
	}
	if err := full.change(ctx, now, func([]SenderBlock) ([]SenderBlock, error) { return blocks, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := full.Put(ctx, manual("domain", "one-more.test"), own, now); !errors.Is(err, ErrBlockFull) {
		t.Fatal("cap exceeded", err)
	}
	if _, err := full.Put(ctx, manual("domain", "d4000.test"), own, now); err != nil {
		t.Fatal("replacing an existing block at the cap", err)
	}

	// Wire budget, measured as SignTable encodes: '&' marshals to six
	// bytes. A list filled to the budget with worst-case values still signs
	// beside routes; the next block is refused.
	long := NewBlocks(t.TempDir())
	worst := func(i int) string { return fmt.Sprintf("%05d", i) + strings.Repeat("&", 300) + "@x.test" }
	n, seed := 250, []SenderBlock{}
	for i := range n {
		seed = append(seed, SenderBlock{ID: BlockID("address", worst(i)), Kind: "address", Value: worst(i), Source: "manual", CreatedAt: 1, Actor: "a", Reason: "spam"})
	}
	if err := long.change(ctx, now, func([]SenderBlock) ([]SenderBlock, error) { return seed, nil }); err != nil {
		t.Fatal(err)
	}
	for ; ; n++ {
		if _, err := long.Put(ctx, manual("address", worst(n)), own, now); errors.Is(err, ErrBlockFull) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	listed, _ := long.List(now)
	wire, size := []cfreceiving.Block{}, 0
	for _, b := range listed {
		wire = append(wire, b.Wire())
		size += cfreceiving.BlockWireBytes(b.Wire())
	}
	if n < 100 || size > maxBlockWire || size+cfreceiving.BlockWireBytes(SenderBlock{Kind: "address", Value: worst(n)}.Wire()) <= maxBlockWire {
		t.Fatal("budget not filled exactly", n, size)
	}
	m, _ := cfreceiving.NewMaterial(1)
	routes := []cfreceiving.Route{}
	for i := range 2000 {
		routes = append(routes, cfreceiving.Route{Address: fmt.Sprintf("user%04d@%s.example.test", i, strings.Repeat("d", 60)), Generation: 1, MaxBytes: 1})
	}
	if _, err := cfreceiving.SignTable(m, 1<<53-1, 1<<53-1, routes, wire); err != nil {
		t.Fatal("full block budget does not sign beside 2000 routes", err)
	}
	// A list over the budget is malformed on read (backups refuse it too).
	raw, _ = os.ReadFile(long.path())
	extra := SenderBlock{Kind: "address", Value: worst(n), Source: "manual", CreatedAt: 1, Actor: "a", Reason: "spam"}
	extra.ID = BlockID(extra.Kind, extra.Value)
	entry, _ := json.Marshal(extra)
	over := append(append(raw[:len(raw)-2:len(raw)-2], ','), append(entry, ']', '}')...)
	if _, err := ParseBlocks(over); err == nil {
		t.Fatal("over-budget list parsed")
	}
	if _, err := ParseBlocks(raw); err != nil {
		t.Fatal(err)
	}

	// A missing receiving directory is never created; a corrupt list refuses.
	gone := NewBlocks(filepath.Join(t.TempDir(), "receiving"))
	if _, err := gone.Put(ctx, manual("domain", "evil.test"), own, now); !errors.Is(err, ErrBlockStore) {
		t.Fatal("missing directory", err)
	}
	if _, err := os.Stat(gone.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("block write created the receiving directory")
	}
	if list, err := gone.List(now); err != nil || len(list) != 0 {
		t.Fatal("missing list", err)
	}
	if err := os.WriteFile(full.path(), []byte(`{"version":1,"blocks":[{"id":"x","kind":"domain","value":"Evil.test","until":null,"source":"manual","level":0,"createdAt":1,"actor":"a","reason":"spam"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := full.Blocked("a@evil.test", now); err == nil {
		t.Fatal("malformed list accepted")
	}
}

// The fixture shared with the Worker test: normalisation and matching agree.
func TestSenderBlocksMatchWorker(t *testing.T) {
	raw, err := os.ReadFile("../../../receiving-worker/blocks.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Values []struct {
			Kind, Value string
			Stored      *string
		}
		Matches []struct {
			Sender string
			Blocks []cfreceiving.Block
			Result string
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Values) == 0 || len(fixture.Matches) == 0 {
		t.Fatal(err)
	}
	for _, v := range fixture.Values {
		got, err := NormalizeBlock(v.Kind, v.Value)
		if v.Stored == nil && err == nil || v.Stored != nil && (err != nil || got != *v.Stored) {
			t.Errorf("normalize %s %q = %q %v", v.Kind, v.Value, got, err)
		}
	}
	ctx, now := context.Background(), time.Now()
	for _, m := range fixture.Matches {
		s := NewBlocks(t.TempDir())
		for _, b := range m.Blocks {
			kind, value := "address", b.Address
			if b.Domain != "" {
				kind, value = "domain", b.Domain
			}
			if _, err := s.Put(ctx, SenderBlock{Kind: kind, Value: value, Source: "manual", Actor: "t", Reason: "spam"}, nil, now); err != nil {
				t.Fatal(m.Sender, value, err)
			}
		}
		blocked, err := s.Blocked(m.Sender, now)
		got := map[bool]string{true: "blocked", false: "allowed"}[blocked]
		if !cfreceiving.ValidSender(m.Sender) {
			got = "refused"
		}
		if err != nil || got != m.Result {
			t.Errorf("sender %q: %s, want %s (%v)", m.Sender, got, m.Result, err)
		}
	}
}
