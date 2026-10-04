package ingress

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHoldingPhysicalAdmissionPreservesRecovery(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	route := proofRoute("one@example.test", "one", 1)
	for _, err := range []error{s.SetRoute(ctx, route), s.Bind(ctx, "maddy", "accepted", "", route.Address), s.Bind(ctx, "maddy", "staged", "", route.Address), s.Accept(ctx, "maddy", "accepted", "", strings.NewReader("exact accepted mail"))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Close new admission without filling the machine's shared filesystem.
	s.physicalLimit = 1
	for _, err := range []error{s.SetRoute(ctx, route), s.Bind(ctx, "maddy", "new", "", route.Address), s.Accept(ctx, "maddy", "staged", "", strings.NewReader("new bytes"))} {
		if !errors.Is(err, ErrCapacity) {
			t.Fatalf("growth not refused: %v", err)
		}
	}
	for _, err := range []error{s.Bind(ctx, "maddy", "accepted", "", route.Address), s.Accept(ctx, "maddy", "accepted", "", strings.NewReader("exact accepted mail"))} {
		if err != nil {
			t.Fatalf("exact replay refused: %v", err)
		}
	}
	for _, change := range []func(*Route){func(r *Route) { r.Address = "new@example.test" }, func(r *Route) { r.Subject = "foreign" }, func(r *Route) { r.Issuer = "https://foreign.test" }, func(r *Route) { r.Mailbox = "foreign" }, func(r *Route) { r.Generation++ }, func(r *Route) { r.Active = false }} {
		bad := route
		change(&bad)
		if err := s.RefreshRoute(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("recovery changed authority: %v", err)
		}
	}
	route.ValidUntil = time.Now().Add(-time.Minute)
	if err := s.RefreshRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	route.ValidUntil = time.Now().Add(time.Minute)
	if err := s.RefreshRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	d, err := s.Claim(ctx, "maddy", "accepted", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Raw) != "exact accepted mail" {
		t.Fatal("accepted bytes changed")
	}
	if err := s.Acknowledge(ctx, "maddy", "accepted", d.Lease, d.Digest); err != nil {
		t.Fatal(err)
	}
	staged, err := s.Get(ctx, "maddy", "staged")
	if err != nil || staged.State != "staged" || len(staged.Raw) != 0 {
		t.Fatalf("failed admission changed staged receipt: %+v %v", staged, err)
	}
}

func TestHoldingPinnedWALRefusesAndRecovers(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "holding")
	s, err := Open(dir, proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	route := proofRoute("one@example.test", "one", 1)
	if err := s.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	// Real independent SQLite reader pins an old snapshot just like a backup.
	reader, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	snapshot, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	var count int
	if err := snapshot.QueryRow("SELECT COUNT(*) FROM routes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	// Avoid five seconds per busy checkpoint in this test only.
	if _, err := s.db.Exec("PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte("x"), 1<<20)
	refused := false
	for i := 0; i < proofLimits.Records; i++ {
		id := fmt.Sprintf("wal-%d", i)
		if err := s.Bind(ctx, "maddy", id, "", route.Address); err != nil {
			if errors.Is(err, ErrCapacity) {
				refused = true
				break
			}
			t.Fatal(err)
		}
		err := s.Accept(ctx, "maddy", id, "", bytes.NewReader(raw))
		if errors.Is(err, ErrCapacity) {
			refused = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.Claim(ctx, "maddy", id, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(d.Raw, raw) {
			t.Fatal("WAL pressure changed accepted bytes")
		}
		if err := s.Acknowledge(ctx, "maddy", id, d.Lease, d.Digest); err != nil {
			t.Fatal(err)
		}
	}
	if !refused {
		t.Fatal("pinned physical WAL did not close admission")
	}
	if err := snapshot.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The next admission checkpoints released pages automatically.
	if err := s.SetRoute(ctx, route); err != nil {
		t.Fatalf("released reader stranded admission: %v", err)
	}
	used, err := s.physicalBytes()
	if err != nil || used >= s.physicalLimit {
		t.Fatalf("checkpoint failed to reclaim WAL: bytes=%d error=%v", used, err)
	}
}

func TestHoldingPhysicalLimitsAndFreeSpace(t *testing.T) {
	for _, limits := range []Limits{{MessageBytes: 1, PayloadBytes: math.MaxInt64, Records: 1}, {MessageBytes: 1, PayloadBytes: 1, Records: math.MaxInt}} {
		if s, err := Open(filepath.Join(t.TempDir(), "holding"), limits); err == nil {
			s.Close()
			t.Fatal("overflowing limits accepted")
		}
	}
	dir := t.TempDir()
	if err := receivingFreeSpace(dir, 1); err != nil {
		t.Fatal(err)
	}
	if err := receivingFreeSpace(dir, math.MaxUint64); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unavailable reserve accepted: %v", err)
	}
	if err := receivingFreeSpace(filepath.Join(dir, "missing"), 1); err == nil || errors.Is(err, ErrCapacity) {
		t.Fatalf("failed capacity measurement not explicit: %v", err)
	}
}

func TestHoldingPhysicalRefusalAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "holding")
	s, err := Open(dir, proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetRoute(ctx, proofRoute("one@example.test", "one", 1)); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, "ingress.db-shm"), 64<<20); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, id := range []string{"pressure-a", "pressure-b"} {
		go func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMaddyHelper$", "--", root, "bind", id, "", "one@example.test")
			cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
			results <- cmd.Run()
		}()
	}
	for range 2 {
		err := <-results
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("physical pressure did not refuse competing helper: %v", err)
		}
	}
	rows, err := s.List(ctx, "maddy-proof", 0, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused subprocess left admitted receipt: %+v %v", rows, err)
	}
}
