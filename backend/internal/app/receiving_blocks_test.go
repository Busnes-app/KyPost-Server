//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

func putBlock(t *testing.T, stateDir, kind, value string, until *int64) {
	t.Helper()
	if _, err := ingress.NewBlocks(filepath.Join(stateDir, "receiving")).Put(context.Background(), ingress.SenderBlock{Kind: kind, Value: value, Until: until, Source: "manual", Actor: "test", Reason: "spam"}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Maddy RCPT: a blocked address or exact domain exits 6 (SMTP 550) and
// stores nothing; subdomains, other senders, the null sender and expired
// blocks bind normally.
func TestNativeReceivingBlockedSender(t *testing.T) {
	r, _ := receivingFixture(t)
	ctx := context.Background()
	putBlock(t, r.stateDir, "address", "bad@spam.test", nil)
	putBlock(t, r.stateDir, "domain", "evil.test", nil)
	soon := time.Now().Add(500 * time.Millisecond).UnixMilli()
	putBlock(t, r.stateDir, "domain", "brief.test", &soon)
	for i, sender := range []string{"Bad@SPAM.test", "anyone@Evil.Test", "x@brief.test"} {
		err := r.bind(ctx, "blocked-"+string(rune('a'+i)), sender, "one@example.test")
		var commandError *receivingCommandError
		if !errors.As(err, &commandError) || commandError.ExitCode() != 6 || !errors.Is(err, ingress.ErrSenderBlock) {
			t.Fatal("blocked sender bound", sender, err)
		}
	}
	if rows, err := r.holding.List(ctx, receivingGateway, 0, 100); err != nil || len(rows) != 0 {
		t.Fatal("blocked sender stored", rows, err)
	}
	time.Sleep(time.Until(time.UnixMilli(soon)) + 10*time.Millisecond) // let the brief block expire
	for i, sender := range []string{"x@sub.evil.test", "good@spam.test", "", "x@brief.test"} {
		if err := r.bind(ctx, "allowed-"+string(rune('a'+i)), sender, "one@example.test"); err != nil {
			t.Fatal("unblocked sender refused", sender, err)
		}
	}
	// Senders the Worker would refuse exit 7, so every accepted sender is
	// blockable in both profiles; nothing is stored.
	for i, sender := range []string{`"a b"@x.test`, "a@[1.2.3.4]", "a@bücher.test", "a@\u212a.test"} {
		var commandError *receivingCommandError
		if err := r.bind(ctx, "shape-"+string(rune('a'+i)), sender, "one@example.test"); !errors.As(err, &commandError) || commandError.ExitCode() != 7 {
			t.Fatal("unblockable sender bound", sender, err)
		}
	}
	// An unreadable list refuses temporarily (exit 8, 451), never binding unchecked.
	path := filepath.Join(r.stateDir, "receiving", ingress.BlocksFile)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var unreadable *receivingCommandError
	if err := r.bind(ctx, "unreadable", "good@spam.test", "one@example.test"); !errors.As(err, &unreadable) || unreadable.ExitCode() != 8 || !strings.Contains(err.Error(), "sender blocks unreadable") {
		t.Fatal("unreadable block list", err)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if rows, err := r.holding.List(ctx, receivingGateway, 0, 100); err != nil || len(rows) != 4 {
		t.Fatal("refused senders stored", len(rows), err)
	}
	// The command maps the block to exit 6 before any DNS proof.
	quarantineCLI(t, r)
	var commandError *receivingCommandError
	if err := runReceivingCommand([]string{"bind", "via-command", "bad@spam.test", "one@example.test"}, nil); !errors.As(err, &commandError) || commandError.ExitCode() != 6 {
		t.Fatal("command exit", err)
	}
	if err := runReceivingCommand([]string{"bind", "via-command-shape", "Name <a@x.test>", "one@example.test"}, nil); !errors.As(err, &commandError) || commandError.ExitCode() != 7 {
		t.Fatal("malformed sender exit", err)
	}
}

func TestNativeReceivingBlocksCLI(t *testing.T) {
	r, _ := receivingFixture(t)
	cli := func(args ...string) (string, error) {
		var out strings.Builder
		err := runReceivingBlocks(args, &out)
		return out.String(), err
	}
	quarantineCLI(t, r)
	for _, args := range [][]string{
		{"add", "address", "bad@spam.test"},
		{"add", "address", "bad@spam.test", "--confirm", "other@spam.test"},
		{"add", "address", "bad@spam.test", "--yes", "bad@spam.test"},
		{"add", "user", "bad@spam.test", "--confirm", "bad@spam.test"},
		{"add", "address", "bad@spam.test", "--confirm", "bad@spam.test", "--until", "tomorrow"},
		{"add", "address", "bad@spam.test", "--confirm", "bad@spam.test", "--reason", "my free text"},
		{"add", "domain", "Example.test", "--confirm", "Example.test"},
		{"add", "address", "someone@example.test", "--confirm", "someone@example.test"},
		{"add", "domain", "bücher.test", "--confirm", "bücher.test"},
		{"remove", "domain", "evil.test", "--confirm", "evil.test"},
		{"list", "extra"},
	} {
		if _, err := cli(args...); err == nil {
			t.Fatal("refused command succeeded", args)
		}
	}
	if out, err := cli("list"); err != nil || strings.TrimSpace(out) != `{"blocks":[]}` {
		t.Fatal("refusals changed the list", out, err)
	}
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	out, err := cli("add", "domain", "Evil.test", "--until", until, "--reason", "phishing", "--confirm", "Evil.test")
	var added ingress.SenderBlock
	if err != nil || json.Unmarshal([]byte(out), &added) != nil || added.Value != "evil.test" || added.Until == nil || added.Reason != "phishing" || added.Source != "manual" || !strings.HasPrefix(added.Actor, "cli:") {
		t.Fatal("add", out, err)
	}
	if _, err := cli("add", "address", "bad@spam.test", "--confirm", "bad@spam.test"); err != nil {
		t.Fatal(err)
	}
	out, err = cli("list")
	if err != nil || !strings.Contains(out, `"value":"evil.test"`) || !strings.Contains(out, `"value":"bad@spam.test"`) || !strings.Contains(out, `"reason":"other"`) {
		t.Fatal("list", out, err)
	}
	if out, err = cli("remove", "domain", "EVIL.test", "--confirm", "EVIL.test"); err != nil || strings.TrimSpace(out) != "removed" {
		t.Fatal("remove", out, err)
	}
	if out, err = cli("list"); err != nil || strings.Contains(out, "evil.test") {
		t.Fatal("list after remove", out, err)
	}
	if os.Geteuid() != 0 {
		t.Setenv("STATE_DIR", "/")
		if _, err := cli("list"); err == nil || !strings.Contains(err.Error(), "receiving blocks <command>") {
			t.Fatal("ran as another user than the state owner", err)
		}
	}
}

// Cloudflare: the publisher signs blocks in force into the table, a block
// change republishes at once and moves the loop's change mark.
func TestCloudflareContinuousPublishesBlocks(t *testing.T) {
	e := cfFixture(t)
	e.cycle(t)
	puts, mark := e.w.putCount(), cfChangeMark(e.r)
	until := e.clock.now().Add(time.Hour).UnixMilli()
	putBlock(t, e.r.stateDir, "domain", "evil.test", nil)
	putBlock(t, e.r.stateDir, "address", "bad@spam.test", &until)
	if cfChangeMark(e.r) == mark {
		t.Fatal("block change does not trigger a publish")
	}
	e.cycle(t)
	table := e.w.lastPut()
	got, _ := json.Marshal(table.BlockedSenders)
	want := `[{"address":"bad@spam.test","until":` + jsonInt(until) + `},{"domain":"evil.test","until":null}]`
	if e.w.putCount() != puts+1 || string(got) != want {
		t.Fatal("blocks not republished", e.w.putCount()-puts, string(got))
	}
	// Unchanged blocks do not republish; removal does.
	e.cycle(t)
	if e.w.putCount() != puts+1 {
		t.Fatal("unchanged blocks republished")
	}
	if _, err := ingress.NewBlocks(e.receiving).Remove(context.Background(), ingress.BlockID("domain", "evil.test"), time.Now()); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if got, _ = json.Marshal(e.w.lastPut().BlockedSenders); e.w.putCount() != puts+2 || strings.Contains(string(got), "evil.test") {
		t.Fatal("unblock not republished", string(got))
	}
	// An unreadable list keeps publishing routes, with the last published
	// blocks, and reports the error.
	previous, _ := json.Marshal(e.w.lastPut().BlockedSenders)
	if err := os.WriteFile(filepath.Join(e.receiving, ingress.BlocksFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(2 * time.Hour)
	e.cycle(t)
	got, _ = json.Marshal(e.w.lastPut().BlockedSenders)
	if s := cfStatus(t, e); e.w.putCount() != puts+3 || string(got) != string(previous) || s.State != "error" || !strings.Contains(s.Detail, "sender block list") || !strings.Contains(s.Detail, "last published blocks") {
		t.Fatalf("unreadable blocks: puts %d blocks %s status %+v", e.w.putCount()-puts, got, s)
	}
	// An unchanged table is not re-sent, but the error stays reported.
	e.cycle(t)
	if s := cfStatus(t, e); e.w.putCount() != puts+3 || s.State != "error" || !strings.Contains(s.Detail, "sender block list") {
		t.Fatalf("unchanged cycle hid the error: puts %d status %+v", e.w.putCount()-puts, s)
	}
	// A route change still publishes while the list stays unreadable.
	receivingDirectory(t, e.r, "two", 2, false)
	e.cycle(t)
	if got, _ = json.Marshal(e.w.lastPut().BlockedSenders); e.w.putCount() != puts+4 || len(e.w.lastPut().Routes) != 1 || string(got) != string(previous) {
		t.Fatalf("route change not published: puts %d routes %d blocks %s", e.w.putCount()-puts, len(e.w.lastPut().Routes), got)
	}
}

// With no previous publish, an unreadable list publishes no blocks.
func TestCloudflareContinuousUnreadableBlocksFirstPublish(t *testing.T) {
	e := cfFixture(t)
	if err := os.WriteFile(filepath.Join(e.receiving, ingress.BlocksFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if s := cfStatus(t, e); e.w.putCount() != 1 || len(e.w.lastPut().Routes) != 2 || e.w.lastPut().BlockedSenders == nil || len(e.w.lastPut().BlockedSenders) != 0 || s.State != "error" || !strings.Contains(s.Detail, "sender block list") {
		t.Fatalf("first publish: puts %d %+v status %+v", e.w.putCount(), e.w.lastPut(), s)
	}
}

func jsonInt(v int64) string { raw, _ := json.Marshal(v); return string(raw) }

// Blocks that do not fit beside the routes are left out (automatic, then
// oldest first) and reported; routes still publish.
func TestCloudflareContinuousTruncatesBlocks(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	store := ingress.NewBlocks(e.receiving)
	now := e.clock.now()
	put := func(value, source string, level int, at time.Time) {
		t.Helper()
		if _, err := store.Put(ctx, ingress.SenderBlock{Kind: "domain", Value: value, Source: source, Level: level, Actor: "t", Reason: "spam"}, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	put("auto.test", "automatic", 1, now.Add(time.Minute))
	put("old-manual.test", "manual", 0, now)
	put("new-manual.test", "manual", 0, now.Add(time.Second))
	got, err := e.l.blockedSenders()
	if err != nil || len(got) != 3 || got[0].Domain != "new-manual.test" || got[1].Domain != "old-manual.test" || got[2].Domain != "auto.test" {
		t.Fatalf("priority order %+v %v", got, err)
	}
	// An over-limit list reaches the publisher through the last-installed
	// fallback (the store itself refuses one): 6000 recorded blocks.
	e.cycle(t)
	rev, _, _, err := e.db.LastInstalled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	many := []cfreceiving.Block{}
	for i := range 6000 {
		many = append(many, cfreceiving.Block{Domain: "d" + strconv.Itoa(i) + ".test"})
	}
	routes, _, err := e.db.Routes(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	list := slices.Collect(maps.Values(routes))
	if err := e.db.Record(ctx, rev+1, rev+1, "over-limit", list, many); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Installed(ctx, rev+1, rev+1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.receiving, ingress.BlocksFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	puts := e.w.putCount()
	e.l = e.restart()
	e.cycle(t)
	table := e.w.lastPut()
	if s := cfStatus(t, e); e.w.putCount() != puts+1 || len(table.Routes) != 2 || len(table.BlockedSenders) != 5000 || s.State != "error" || !strings.Contains(s.Detail, "1000 sender blocks do not fit") {
		t.Fatalf("over-limit blocks: puts %d routes %d blocks %d status %+v", e.w.putCount()-puts, len(table.Routes), len(table.BlockedSenders), s)
	}
}
