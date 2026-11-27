package mailbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

func TestTableOutboundIgnoresHostLocalIdleFiles(t *testing.T) {
	dbbackend.RequirePostgres(t)
	root := t.TempDir()
	db, err := volumes.Open(root, true)
	must(t, err)
	defer db.Close()
	table, err := vol.NewTable(db, "state", vol.TableOptions{MasterKey: bytes.Repeat([]byte{3}, 32), Org: func(context.Context, string) (string, error) { return "", nil }})
	must(t, err)
	unmount, err := vol.Mount(root, table)
	must(t, err)
	defer unmount()
	dir := filepath.Join(root, "mailbox")
	first := openTest(t, dir, testOwner, testLimits)
	second := openTest(t, dir, testOwner, testLimits)
	// A different replica cannot clear a stale mark on this host. A directory
	// at the old lock path also proves enqueue/claim never opens that file.
	mark := []byte("retained host-local idle hint")
	must(t, os.MkdirAll(dir, 0700))
	must(t, os.WriteFile(filepath.Join(dir, OutboundIdleFile), mark, 0600))
	must(t, os.Mkdir(filepath.Join(dir, OutboundIdleFile+".lock"), 0700))
	if probe(t, dir) {
		t.Fatal("empty table mailbox has work")
	}
	must(t, first.QueueOutbound(t.Context(), outboxMaster, outboxTestID, outboxTestJob()))
	if !probe(t, dir) {
		t.Fatal("host-local hint hid table work")
	}
	start := make(chan struct{})
	type result struct {
		claim string
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, writer := range []*Store{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, claim, err := writer.ClaimOutbound(context.Background(), outboxMaster, outboxTestID, 0, outboxGeneration)
			results <- result{claim, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	var claim string
	for r := range results {
		if r.err == nil {
			winners++
			claim = r.claim
		} else if !errors.Is(r.err, ErrOutbound) {
			t.Fatal("claim refused for unrelated reason", r.err)
		}
	}
	if winners != 1 {
		t.Fatal("claim winners", winners)
	}
	if probe(t, dir) {
		t.Fatal("claimed delivery scheduled again")
	}
	must(t, second.CompleteOutbound(t.Context(), outboxMaster, outboxTestID, 0, claim, nil))
	if !probe(t, dir) {
		t.Fatal("accepted delivery lost its Sent reservation")
	}
	_, err = first.FileOutboundSent(t.Context(), outboxMaster, outboxTestID)
	must(t, err)
	if probe(t, dir) {
		t.Fatal("completed Sent job still pending")
	}
	raw, err := os.ReadFile(filepath.Join(dir, OutboundIdleFile))
	must(t, err)
	if !bytes.Equal(raw, mark) {
		t.Fatal("table writer changed local hint")
	}
	if _, err := os.Stat(filepath.Join(dir, OutboundIdleFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("table writer created temporary hint", err)
	}
}

func TestUnmigratedPostgresOutboundKeepsIdleLock(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dir := filepath.Join(t.TempDir(), "mailbox")
	writer := openTest(t, dir, testOwner, testLimits)
	mark := filepath.Join(dir, OutboundIdleFile)
	must(t, os.WriteFile(mark, []byte("old local hint"), 0600))
	must(t, writer.QueueOutbound(t.Context(), outboxMaster, outboxTestID, outboxTestJob()))
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatal("ordinary writer did not clear local hint", err)
	}
	info, err := os.Stat(filepath.Join(dir, OutboundIdleFile+".lock"))
	must(t, err)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("ordinary lock changed", info.Mode())
	}
}
