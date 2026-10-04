//go:build linux

package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestHoldingCheckpointHeadroom(t *testing.T) {
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("private mount namespaces unavailable")
	}
	if output, err := exec.Command(unshare, "--user", "--map-root-user", "--mount", "true").CombinedOutput(); err != nil {
		t.Skipf("private mount namespaces unavailable: %s", output)
	}
	// This mount exists only inside the child's namespace; never fill a shared
	// host filesystem to simulate exhaustion. 77 means mounting is unavailable.
	cmd := exec.Command(unshare, "--user", "--map-root-user", "--mount", "sh", "-c", `mount -t tmpfs -o size=48m tmpfs "$1" || exit 77
exec "$2" -test.run=^TestHoldingCheckpointHeadroomHelper$ -- "$1"`, "sh", t.TempDir(), os.Args[0])
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 77 {
		t.Skipf("private tmpfs mount unavailable: %s", output)
	}
	if err != nil {
		t.Fatalf("private size-limited checkpoint regression: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS") {
		t.Fatalf("checkpoint helper did not execute: %s", output)
	}
}

func TestHoldingCheckpointHeadroomHelper(t *testing.T) {
	var root string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			root = os.Args[i+1]
			break
		}
	}
	if root == "" {
		return
	}
	ctx := context.Background()
	limits := Limits{MessageBytes: 1 << 20, PayloadBytes: 32 << 20, Records: 30}
	s, err := Open(filepath.Join(root, "holding"), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	route := proofRoute("one@example.test", "one", 1)
	if err := s.SetRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte("x"), 1<<20)
	for i := range 28 {
		id := fmt.Sprintf("uncheckpointed-%d", i)
		if err := s.Bind(ctx, "maddy", id, "", route.Address); err != nil {
			t.Fatal(err)
		}
		if err := s.Accept(ctx, "maddy", id, "", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	// Close admission with a WAL that contains main-file growth larger than
	// remaining space. The old unconditional checkpoint exhausts this tmpfs.
	s.physicalLimit = 32 << 20
	var before syscall.Statfs_t
	if err := syscall.Statfs(root, &before); err != nil {
		t.Fatal(err)
	}
	main, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var pages, pageSize int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	available := int64(before.Bavail) * before.Bsize
	if pages*pageSize-main.Size() <= available || available < 16<<20 {
		t.Fatalf("fixture lacks checkpoint pressure or reserve: growth=%d available=%d", pages*pageSize-main.Size(), available)
	}
	admissionErr := s.Bind(ctx, "maddy", "new", "", route.Address)
	var refused syscall.Statfs_t
	if err := syscall.Statfs(root, &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Bavail < before.Bavail {
		t.Fatalf("refused admission consumed recovery space: before=%d after=%d error=%v", before.Bavail, refused.Bavail, admissionErr)
	}
	if !errors.Is(admissionErr, ErrCapacity) {
		t.Fatalf("new admission did not refuse safely: %v", admissionErr)
	}
	for _, err := range []error{s.Bind(ctx, "maddy", "uncheckpointed-0", "", route.Address), s.Accept(ctx, "maddy", "uncheckpointed-0", "", bytes.NewReader(raw))} {
		if err != nil {
			t.Fatalf("exact replay lost at checkpoint refusal: %v", err)
		}
	}
	var after syscall.Statfs_t
	if err := syscall.Statfs(root, &after); err != nil {
		t.Fatal(err)
	}
	if after.Bavail < before.Bavail {
		t.Fatalf("refused admission consumed recovery space: before=%d after=%d", before.Bavail, after.Bavail)
	}
	unchanged, err := os.Stat(s.path)
	if err != nil || unchanged.Size() != main.Size() {
		t.Fatalf("unsafe checkpoint grew main DB: %v", err)
	}
	d, err := s.Get(ctx, "maddy", "uncheckpointed-0")
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, raw) {
		t.Fatalf("checkpoint refusal changed accepted mail: state=%s error=%v", d.State, err)
	}
}
