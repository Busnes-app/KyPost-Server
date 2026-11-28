//go:build linux

package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

// Multi mode keeps sender blocks and evidence in tenancy.db (RX R7) and never opens the
// files: with both file names taken by unreadable directories, a file read would answer
// bind with exit 8. The CLI and bind over TLS share the table list.
func TestReceivingMultiModeBlocksLiveInTenancy(t *testing.T) {
	f := multiReceivingFixture(t)
	dir := filepath.Join(f.r.stateDir, "receiving")
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		if err := os.Mkdir(filepath.Join(dir, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	r, err := openReceivingRuntime(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close() })
	if r.tenancy == nil {
		t.Fatal("multi runtime without tenancy.db")
	}
	f.r.senders = r.senders // the fixture's runtime keeps its DNS stub
	addr, _ := f.serve(t)
	key, _ := f.key(t, "a", "mx-a")
	f.remote(t, key, []string{addr})

	if err := runReceivingBlocks([]string{"add", "domain", "outside.test", "--confirm", "outside.test"}, io.Discard); err != nil {
		t.Fatal("block add:", err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM sender_blocks WHERE id = ?`, ingress.BlockID("domain", "outside.test")).Scan(&n); err != nil || n != 1 {
		t.Fatal("block not in tenancy.db", n, err)
	}
	if code := exitCode(runReceivingCommand([]string{"bind", "tx-1", tlsSender, "one@example.test"}, nil)); code != 6 {
		t.Fatalf("blocked sender answered %d, want 6", code)
	}
	if err := runReceivingCommand([]string{"bind", "tx-2", "friend@inside.test", "one@example.test"}, nil); err != nil {
		t.Fatalf("allowed sender answered %d (%v), want 0", exitCode(err), err)
	}
	var out bytes.Buffer
	if err := runReceivingBlocks([]string{"list"}, &out); err != nil || !bytes.Contains(out.Bytes(), []byte(`"outside.test"`)) {
		t.Fatal("list:", out.String(), err)
	}
	if err := runReceivingBlocks([]string{"remove", "domain", "outside.test", "--confirm", "outside.test"}, io.Discard); err != nil {
		t.Fatal("block remove:", err)
	}
	if code := exitCode(runReceivingCommand([]string{"bind", "tx-3", tlsSender, "one@example.test"}, nil)); code != 0 {
		t.Fatalf("unblocked sender answered %d, want 0", code)
	}
	// The unblock's suppression is a tenancy.db row too.
	if err := f.db.QueryRow(`SELECT count(*) FROM sender_evidence WHERE kind = 'unblocked'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("unblock not recorded in tenancy.db", n, err)
	}
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || !info.IsDir() || info.Mode().Perm() != 0 {
			t.Fatal("multi mode touched", name, err)
		}
	}
}

// A replica in multi mode without its multi record refuses rather than fall back to the
// files.
func TestReceivingMultiModeWithoutTenancyRefuses(t *testing.T) {
	r, _ := receivingFixture(t)
	quarantineCLI(t, r)
	t.Setenv("KYPOST_TENANCY", config.TenancyMulti)
	setObjectStorageEnv(t)
	t.Setenv("KY_CELLS", "test-1")
	t.Setenv("KYPOST_STAFF_ORG_ID", tlsStaffOrg)
	if err := runReceivingBlocks([]string{"list"}, io.Discard); err == nil {
		t.Fatal("multi mode without a multi record listed blocks")
	}
	if _, err := os.Lstat(filepath.Join(r.stateDir, "receiving", ingress.BlocksFile)); !os.IsNotExist(err) {
		t.Fatal("a block file appeared", err)
	}
}
