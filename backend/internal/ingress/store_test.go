package ingress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var proofLimits = Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 20}

func proofRoute(address, owner string, generation int64) Route {
	return Route{Address: address, Issuer: "https://identity.example.test", Subject: owner, Mailbox: owner, Generation: generation, Active: true, ValidUntil: time.Now().Add(time.Hour)}
}

func TestHoldingStore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "holding")
	s, err := Open(dir, proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	require(s.SetRoute(ctx, proofRoute("alias@example.test", "alice", 1)))
	require(s.SetRoute(ctx, proofRoute("hidden@example.test", "bob", 1)))
	if err := s.Bind(ctx, "maddy", "unknown", "sender@outside.test", "unknown@example.test"); !errors.Is(err, ErrRoute) {
		t.Fatalf("unknown: %v", err)
	}
	require(s.Bind(ctx, "maddy", "old", "sender@outside.test", "alias@example.test"))
	require(s.Bind(ctx, "maddy", "old", "sender@outside.test", "hidden@example.test"))
	raw := []byte("From: sender@outside.test\r\nMessage-ID: <sender-chosen>\r\n\r\nbytes\x00retained\r\n")
	require(s.Accept(ctx, "maddy", "old", "sender@outside.test", bytes.NewReader(raw)))
	require(s.Accept(ctx, "maddy", "old", "sender@outside.test", bytes.NewReader(raw)))
	if err := s.Accept(ctx, "maddy", "old", "sender@outside.test", strings.NewReader("changed")); !errors.Is(err, ErrConflict) {
		t.Fatalf("digest conflict: %v", err)
	}
	if err := s.Bind(ctx, "other-gateway", "old", "sender@outside.test", "alias@example.test"); err != nil {
		t.Fatal(err)
	}
	require(s.Close())
	s, err = Open(dir, proofLimits)
	require(err)
	d, err := s.Get(ctx, "maddy", "old")
	require(err)
	if !bytes.Equal(d.Raw, raw) || len(d.Bindings) != 2 || d.Bindings[0].Mailbox != "alice" {
		t.Fatalf("reopen binding/MIME: %+v", d)
	}
	// Age both accepted and live staged mail: a new RCPT must not sweep either
	// to make room, because age is not evidence of SMTP abandonment.
	_, err = s.db.Exec("UPDATE deliveries SET created=0 WHERE gateway='maddy'")
	require(err)
	require(s.Bind(ctx, "maddy", "fresh", "sender@outside.test", "alias@example.test"))
	_, err = s.db.Exec("UPDATE deliveries SET created=0 WHERE gateway='maddy' AND id='fresh'")
	require(err)
	require(s.Bind(ctx, "maddy", "fresh", "sender@outside.test", "hidden@example.test"))
	fresh, err := s.Get(ctx, "maddy", "fresh")
	require(err)
	if len(fresh.Bindings) != 2 || fresh.Bindings[0].Mailbox != "alice" {
		t.Fatal("aged live SMTP transaction lost an accepted RCPT")
	}
	d, err = s.Get(ctx, "maddy", "old")
	require(err)
	if !bytes.Equal(d.Raw, raw) {
		t.Fatal("accepted mail expired")
	}
	// Alias changes between RCPT and pickup must never transfer old mail.
	require(s.SetRoute(ctx, proofRoute("alias@example.test", "carol", 2)))
	if err := s.SetRoute(ctx, proofRoute("alias@example.test", "alice", 1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale route: %v", err)
	}
	if err := s.SetRoute(ctx, proofRoute("alias@example.test", "mallory", 2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-revision conflict: %v", err)
	}
	d, err = s.Claim(ctx, "maddy", "old", time.Minute)
	if !errors.Is(err, ErrRoute) || d.State != "quarantined" || d.Bindings[0].Mailbox != "alice" {
		t.Fatalf("reassignment: %+v %v", d, err)
	}
	require(s.Bind(ctx, "maddy", "new", "", "alias@example.test")) // Null return path.
	require(s.Accept(ctx, "maddy", "new", "", bytes.NewReader(raw)))
	d, err = s.Claim(ctx, "maddy", "new", time.Minute)
	require(err)
	if d.Bindings[0].Mailbox != "carol" {
		t.Fatal("fresh ownership missing")
	}
	if _, err := s.Claim(ctx, "maddy", "new", time.Minute); !errors.Is(err, ErrLease) {
		t.Fatalf("double claim: %v", err)
	}
	if err := s.Acknowledge(ctx, "maddy", "new", "wrong", d.Digest); !errors.Is(err, ErrLease) {
		t.Fatalf("wrong lease: %v", err)
	}
	_, err = s.db.Exec("UPDATE deliveries SET lease_until=0 WHERE gateway='maddy' AND id='new'")
	require(err)
	if err := s.Acknowledge(ctx, "maddy", "new", d.Lease, d.Digest); !errors.Is(err, ErrLease) {
		t.Fatalf("expired lease: %v", err)
	}
	oldLease := d.Lease
	d, err = s.Claim(ctx, "maddy", "new", time.Minute)
	require(err)
	if d.Lease == oldLease {
		t.Fatal("lease reused")
	}
	require(s.Acknowledge(ctx, "maddy", "new", d.Lease, d.Digest))
	require(s.Acknowledge(ctx, "maddy", "new", d.Lease, d.Digest))   // Lost local ACK reply.
	require(s.Accept(ctx, "maddy", "new", "", bytes.NewReader(raw))) // Never resurrect raw.
	got, err := s.Get(ctx, "maddy", "new")
	require(err)
	if got.State != "imported" || len(got.Raw) != 0 || got.Digest != d.Digest {
		t.Fatal("import receipt not retained")
	}
	var cursor int64
	var listed []Summary
	for {
		page, err := s.List(ctx, "maddy", cursor, 1)
		require(err)
		if len(page) == 0 {
			break
		}
		listed = append(listed, page...)
		cursor = page[0].Sequence
	}
	if len(listed) != 3 || listed[0].ID != "old" || listed[0].State != "quarantined" || listed[2].State != "imported" {
		t.Fatalf("scoped receipt pages: %+v", listed)
	}
}

func TestHoldingLimitsAndRefusal(t *testing.T) {
	ctx := context.Background()
	limits := Limits{MessageBytes: 8, PayloadBytes: 8, Records: 3}
	s, err := Open(filepath.Join(t.TempDir(), "holding"), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := proofRoute("a@example.test", "alice", 1)
	if err := s.SetRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := s.Bind(ctx, "g", id, "", r.Address); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Accept(ctx, "g", "1", "", strings.NewReader("12345678")); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"x", "123456789"} {
		if err := s.Accept(ctx, "g", "2", "", strings.NewReader(raw)); !errors.Is(err, ErrCapacity) {
			t.Fatalf("quota/size: %v", err)
		}
	}
	d, err := s.Get(ctx, "g", "2")
	if err != nil || d.State != "staged" || len(d.Raw) > 0 {
		t.Fatalf("refusal left payload: %+v %v", d, err)
	}
	r = proofRoute("a@example.test", "alice", 2)
	r.Active = false
	if err := s.SetRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, "g", "3", "", r.Address); !errors.Is(err, ErrRoute) {
		t.Fatalf("disabled: %v", err)
	}
	r = proofRoute("a@example.test", "alice", 3)
	r.ValidUntil = time.Now().Add(-time.Hour)
	if err := s.SetRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, "g", "3", "", r.Address); !errors.Is(err, ErrRoutingStale) {
		t.Fatalf("stale: %v", err)
	}
}

// TestMaddyHelper is exclusively a subprocess entry for the pinned gateway
// experiment. No runtime mode invokes it. SMTP values remain separate argv
// elements, and stdout must stay empty so Maddy adds no helper-authored headers.
func TestMaddyHelper(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	if len(args) < 4 {
		os.Exit(1)
	}
	s, err := Open(filepath.Join(args[0], "holding"), proofLimits)
	if err != nil {
		os.Exit(1)
	}
	switch args[1] {
	case "bind":
		if len(args) != 5 {
			os.Exit(1)
		}
		// Enforce this experiment's served domain before making any binding.
		// A successful Bind must not precede a later domain-routing rejection.
		if !strings.HasSuffix(args[4], "@example.test") {
			os.Exit(3)
		}
		err = s.Bind(context.Background(), "maddy-proof", args[2], args[3], args[4])
	case "accept":
		// The receiver owns transaction identity. Its {rcpts} includes refused
		// attempts in this release, so only durable successful Bind rows define
		// recipients; this config uses Bind as its sole recipient authority.
		err = s.Accept(context.Background(), "maddy-proof", args[2], args[3], io.LimitReader(os.Stdin, proofLimits.MessageBytes+1))
	default:
		os.Exit(1)
	}
	if err == nil && args[1] == "accept" {
		if _, hookErr := os.Stat(filepath.Join(args[0], "kill-after-commit")); hookErr == nil {
			if err := os.WriteFile(filepath.Join(args[0], "committed"), []byte(strconv.Itoa(os.Getpid())+"\n"+args[2]), 0o600); err != nil {
				os.Exit(1)
			}
			time.Sleep(time.Minute) // Test kills this process with its SQLite handle open.
		}
	}
	_ = s.Close()
	if errors.Is(err, ErrRoute) {
		os.Exit(3)
	}
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestConcurrentHoldingWriters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "holding")
	limits := Limits{MessageBytes: 8, PayloadBytes: 8, Records: 1}
	a, err := Open(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := a.SetRoute(ctx, proofRoute("a@example.test", "alice", 1)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, s := range []*Store{a, b} {
		wg.Go(func() { results <- s.Bind(ctx, "g", strconv.Itoa(i), "", "a@example.test") })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("quota admitted %d writers", accepted)
	}
	var id string
	if err := a.db.QueryRow("SELECT id FROM deliveries").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := a.Accept(ctx, "g", id, "", strings.NewReader("12345678")); err != nil {
		t.Fatal(err)
	}
	results = make(chan error, 2)
	for _, s := range []*Store{a, b} {
		wg.Go(func() { _, err := s.Claim(ctx, "g", id, time.Minute); results <- err })
	}
	wg.Wait()
	close(results)
	claimed := 0
	for err := range results {
		if err == nil {
			claimed++
		} else if !errors.Is(err, ErrLease) {
			t.Fatal(err)
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claim admitted %d importers", claimed)
	}
}

func TestHoldingQuotaAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetRoute(ctx, proofRoute("a@example.test", "alice", 1)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < proofLimits.Records-1; i++ {
		if err := s.Bind(ctx, "maddy-proof", strconv.Itoa(i), "", "a@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"process-a", "process-b"} {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMaddyHelper$", "--", root, "bind", id, "", "a@example.test")
			cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
			results <- cmd.Run()
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("unexpected subprocess failure: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("cross-process quota admitted %d receivers", success)
	}
}
