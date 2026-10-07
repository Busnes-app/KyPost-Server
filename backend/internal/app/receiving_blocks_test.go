//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// The command maps the block to exit 6 before any DNS proof.
	quarantineCLI(t, r)
	var commandError *receivingCommandError
	if err := runReceivingCommand([]string{"bind", "via-command", "bad@spam.test", "one@example.test"}, nil); !errors.As(err, &commandError) || commandError.ExitCode() != 6 {
		t.Fatal("command exit", err)
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
	if _, err := ingress.NewBlocks(e.receiving).Remove(context.Background(), "domain", "evil.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if got, _ = json.Marshal(e.w.lastPut().BlockedSenders); e.w.putCount() != puts+2 || strings.Contains(string(got), "evil.test") {
		t.Fatal("unblock not republished", string(got))
	}
	// An unreadable list never publishes a table without its blocks.
	if err := os.WriteFile(filepath.Join(e.receiving, ingress.BlocksFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(2 * time.Hour)
	e.cycle(t)
	if s := cfStatus(t, e); e.w.putCount() != puts+2 || s.State != "error" || !strings.Contains(s.Detail, "sender block list") {
		t.Fatalf("published without the block list: %d %+v", e.w.putCount()-puts, s)
	}
}

func jsonInt(v int64) string { raw, _ := json.Marshal(v); return string(raw) }
