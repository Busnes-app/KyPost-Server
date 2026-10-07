package ingress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSenderBlocks(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1790000000000)
	s := NewBlocks(t.TempDir())
	own := []string{"example.test"}
	manual := func(kind, value string) SenderBlock {
		return SenderBlock{Kind: kind, Value: value, Source: "manual", Actor: "admin-1", Reason: "spam"}
	}

	// Normalisation: lowercase ASCII; non-ASCII, malformed and unknown kinds refused.
	for in, want := range map[[2]string]string{{"address", "Bad.Guy@SPAM.Test"}: "bad.guy@spam.test", {"domain", "Evil.TEST"}: "evil.test", {"domain", "xn--bcher-kva.test"}: "xn--bcher-kva.test"} {
		if got, err := NormalizeBlock(in[0], in[1]); err != nil || got != want {
			t.Fatal("normalize", in, got, err)
		}
	}
	for _, in := range [][2]string{{"domain", "bücher.test"}, {"domain", "\u212aevil.test"}, {"address", "ü@x.test"}, {"address", "a@bücher.test"}, {"domain", "localhost"}, {"domain", "-x.test"}, {"domain", "x_y.test"},
		{"address", "Name <a@x.test>"}, {"address", `"a b"@x.test`}, {"address", "a@[127.0.0.1]"}, {"address", strings.Repeat("a", 65) + "@x.test"}, {"address", "x.test"}, {"user", "a@x.test"}, {"domain", ""}} {
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
	// A U-label sender matches its A-label block.
	if _, err := s.Put(ctx, manual("domain", "xn--bcher-kva.test"), own, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Blocked("a@bücher.test", now); !got {
		t.Fatal("U-label sender dodged the A-label block")
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

	// Wire budget: long values fill half the Worker's table before the count cap.
	long := NewBlocks(t.TempDir())
	tail := "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	blocks = nil
	for i := range 1839 {
		v := fmt.Sprintf("%04d", i) + strings.Repeat("a", 59) + tail
		blocks = append(blocks, SenderBlock{ID: BlockID("domain", v), Kind: "domain", Value: v, Source: "manual", CreatedAt: 1, Actor: "a", Reason: "spam"})
	}
	if err := long.change(ctx, now, func([]SenderBlock) ([]SenderBlock, error) { return blocks, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := long.Put(ctx, manual("domain", "9999"+strings.Repeat("a", 59)+tail), own, now); !errors.Is(err, ErrBlockFull) {
		t.Fatal("wire budget exceeded", err)
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
