package mailbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var testOwner = Owner{Issuer: "https://identity.example.test", Subject: "alice", Mailbox: "alice"}
var testLimits = Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 20000}
var testRaw = []byte("From: sender@outside.test\r\nTo: alice@example.test\r\nSubject: opaque mail\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\nexact bytes\x00\r\n")

func testReceipt(id string) Receipt {
	return Receipt{Gateway: "maddy", Delivery: id, Sender: "sender@outside.test", Recipients: []Recipient{{Address: "alice@example.test", Generation: 1}}}
}
func openTest(t *testing.T, dir string, owner Owner, limits Limits) *Store {
	t.Helper()
	s, err := Open(dir, owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMailboxDurabilityAndMutations(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(testRaw))
	must(t, err)
	must(t, s.CreateFolder(ctx, "INBOX/Private"))
	must(t, s.Update(ctx, "INBOX", id, true, true, []string{"$Phishing", "Travel"}))
	_, before, err := s.Changes(ctx, 0, 100)
	must(t, err)
	must(t, s.Update(ctx, "INBOX", id, true, true, []string{"Travel", "$Phishing"}))
	changes, high, err := s.Changes(ctx, before, 100)
	must(t, err)
	if len(changes) != 0 || high != before {
		t.Fatal("identical retry emitted change")
	}
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	raw, err := s.Raw(ctx, "INBOX", id)
	must(t, err)
	if !bytes.Equal(raw, testRaw) {
		t.Fatal("raw changed on restart")
	}
	list, err := s.List(ctx, "INBOX", 0, 100)
	must(t, err)
	if len(list) != 1 || list[0].ID != id || !list[0].Seen || !list[0].Starred || len(list[0].Labels) != 2 {
		t.Fatalf("metadata: %+v", list)
	}
	must(t, s.Move(ctx, "INBOX", id, "INBOX/Private"))
	if _, err = s.Raw(ctx, "INBOX", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale folder reference: %v", err)
	}
	raw, err = s.Raw(ctx, "INBOX/Private", id)
	must(t, err)
	if !bytes.Equal(raw, testRaw) {
		t.Fatal("move changed raw")
	}
	changes, _, err = s.Changes(ctx, before, 100)
	must(t, err)
	if len(changes) != 2 || !changes[0].Removed || changes[1].Removed || changes[1].Folder != "INBOX/Private" {
		t.Fatalf("move deltas: %+v", changes)
	}
	if err = s.DeleteFolder(ctx, "INBOX/Private"); err == nil {
		t.Fatal("nonempty folder deleted")
	}
	must(t, s.Delete(ctx, "INBOX/Private", id))
	must(t, s.DeleteFolder(ctx, "INBOX/Private"))
	replay, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(testRaw))
	must(t, err)
	if replay != id {
		t.Fatal("deleted delivery recreated")
	}
	if _, err = s.Raw(ctx, "Trash", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted raw restored: %v", err)
	}
	next, err := s.Import(ctx, testReceipt("two"), bytes.NewReader(testRaw))
	must(t, err)
	if next <= id {
		t.Fatal("deleted ID reused")
	}
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	changes, high, err = s.Changes(ctx, 0, 100)
	must(t, err)
	if len(changes) != 6 || high != changes[len(changes)-1].Revision {
		t.Fatalf("durable changes: %+v %d", changes, high)
	}
	if _, _, err = s.Changes(ctx, high+1, 100); !errors.Is(err, ErrCursor) {
		t.Fatal("future cursor accepted")
	}
}

func TestMailboxImportConflictsAndOwnership(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	r := testReceipt("one")
	r.Recipients = append(r.Recipients, Recipient{Address: "alias@example.test", Generation: 2})
	id, err := s.Import(ctx, r, bytes.NewReader(testRaw))
	must(t, err)
	r.Recipients[0], r.Recipients[1] = r.Recipients[1], r.Recipients[0]
	replay, err := s.Import(ctx, r, bytes.NewReader(testRaw))
	must(t, err)
	if replay != id {
		t.Fatal("reordered bindings duplicated")
	}
	for _, which := range []string{"bytes", "sender", "generation", "recipients"} {
		changed := r
		changed.Recipients = append([]Recipient(nil), r.Recipients...)
		raw := testRaw
		switch which {
		case "bytes":
			raw = append(append([]byte(nil), raw...), byte('x'))
		case "sender":
			changed.Sender = "other@outside.test"
		case "generation":
			changed.Recipients[0].Generation++
		case "recipients":
			changed.Recipients = changed.Recipients[:1]
		}
		if _, err = s.Import(ctx, changed, bytes.NewReader(raw)); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s conflict: %v", which, err)
		}
	}
	other := testOwner
	other.Subject = "bob"
	if wrong, err := Open(dir, other, testLimits); !errors.Is(err, ErrOwner) {
		if wrong != nil {
			_ = wrong.Close()
		}
		t.Fatalf("owner reassignment: %v", err)
	}
	changedLimits := testLimits
	changedLimits.Records++
	if wrong, err := Open(dir, testOwner, changedLimits); err == nil {
		_ = wrong.Close()
		t.Fatal("quota changed silently")
	}
	if _, err = s.Import(ctx, testReceipt("malformed"), strings.NewReader("not MIME")); err == nil {
		t.Fatal("invalid RFC5322 accepted")
	}
	if _, err = s.Import(ctx, testReceipt("huge"), strings.NewReader(strings.Repeat("x", int(testLimits.MessageBytes)+1))); err == nil {
		t.Fatal("oversized accepted")
	}
	list, err := s.List(ctx, "INBOX", 0, 100)
	must(t, err)
	if len(list) != 1 {
		t.Fatal("failed imports visible")
	}
}

func TestMailboxConcurrentReceiptsAndQuota(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	limits := testLimits
	limits.Records = 1
	a := openTest(t, dir, testOwner, limits)
	b := openTest(t, dir, testOwner, limits)
	var wg sync.WaitGroup
	ids := make(chan int64, 2)
	errs := make(chan error, 2)
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Import(ctx, testReceipt("same"), bytes.NewReader(testRaw))
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	first, second := <-ids, <-ids
	if first != second || first == 0 {
		t.Fatalf("duplicate: %d %d", first, second)
	}
	must(t, <-errs)
	must(t, <-errs)
	if _, err := a.Import(ctx, testReceipt("other"), bytes.NewReader(testRaw)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("quota: %v", err)
	}
	must(t, a.Delete(ctx, "INBOX", first))
	if _, err := b.Import(ctx, testReceipt("other"), bytes.NewReader(testRaw)); !errors.Is(err, ErrCapacity) {
		t.Fatal("identity history silently evicted")
	}
}

func TestMailboxPagesBeyondCacheWindow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	const total = 11001
	for i := 0; i < total; i++ {
		_, err := s.Import(ctx, testReceipt(fmt.Sprint(i)), bytes.NewReader(testRaw))
		must(t, err)
	}
	var before int64
	count := 0
	for {
		page, err := s.List(ctx, "INBOX", before, 200)
		must(t, err)
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			if before != 0 && m.ID >= before {
				t.Fatal("repeated/unordered ID")
			}
			before = m.ID
			count++
		}
	}
	if count != total {
		t.Fatalf("paged %d/%d", count, total)
	}
	cursor := int64(0)
	count = 0
	for {
		page, high, err := s.Changes(ctx, cursor, 200)
		must(t, err)
		if len(page) == 0 {
			if cursor != high {
				t.Fatal("cursor skipped history")
			}
			break
		}
		for _, c := range page {
			if c.Revision <= cursor {
				t.Fatal("delta ordering")
			}
			cursor = c.Revision
			count++
		}
	}
	if count != total {
		t.Fatalf("history %d/%d", count, total)
	}
}

func TestMailboxFolderRenameAndDraftSent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	must(t, s.CreateFolder(ctx, "Work"))
	must(t, s.CreateFolder(ctx, "Work/Child"))
	must(t, s.CreateFolder(ctx, "Existing"))
	id, err := s.Append(ctx, "Work/Child", bytes.NewReader(testRaw), false)
	must(t, err)
	if err = s.RenameFolder(ctx, "Work", "Existing"); err == nil {
		t.Fatal("rename replaced existing folder")
	}
	_, before, err := s.Changes(ctx, 0, 10)
	must(t, err)
	must(t, s.RenameFolder(ctx, "Work", "Personal"))
	if _, err = s.Raw(ctx, "Work/Child", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("old folder reference resolved")
	}
	raw, err := s.Raw(ctx, "Personal/Child", id)
	must(t, err)
	if !bytes.Equal(raw, testRaw) {
		t.Fatal("rename rebuilt raw")
	}
	changes, _, err := s.Changes(ctx, before, 10)
	must(t, err)
	if len(changes) != 2 || !changes[0].Removed || changes[1].Folder != "Personal/Child" {
		t.Fatalf("rename history: %+v", changes)
	}
	if err = s.DeleteFolder(ctx, "Personal"); err == nil {
		t.Fatal("parent with children deleted")
	}
	for _, folder := range []string{"Drafts", "Sent"} {
		id, err = s.Append(ctx, folder, bytes.NewReader(testRaw), folder == "Drafts")
		must(t, err)
		list, err := s.List(ctx, folder, 0, 10)
		must(t, err)
		if len(list) != 1 || list[0].ID != id || list[0].Draft != (folder == "Drafts") {
			t.Fatalf("stored copy flags: %+v", list)
		}
	}
}

func TestMailboxPayloadQuotaAcrossWriters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	limits := testLimits
	limits.MessageBytes = int64(len(testRaw))
	limits.PayloadBytes = int64(len(testRaw))
	a := openTest(t, dir, testOwner, limits)
	b := openTest(t, dir, testOwner, limits)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Import(ctx, testReceipt(fmt.Sprint(i)), bytes.NewReader(testRaw))
			results <- err
		}()
	}
	wg.Wait()
	success, full := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrCapacity) {
			full++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("payload quota race: success %d full %d", success, full)
	}
	list, err := a.List(ctx, "INBOX", 0, 10)
	must(t, err)
	must(t, a.Delete(ctx, "INBOX", list[0].ID))
	_, err = b.Import(ctx, testReceipt("after-delete"), bytes.NewReader(testRaw))
	must(t, err)
}

func TestMailboxQuotaCountersBackfillAndRollback(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(testRaw))
	must(t, err)
	_, err = s.Import(ctx, testReceipt("two"), bytes.NewReader(testRaw))
	must(t, err)
	must(t, s.Delete(ctx, "INBOX", id))
	must(t, s.Update(ctx, "INBOX", id+1, true, true, []string{"Travel"}))
	must(t, s.Move(ctx, "INBOX", id+1, "Sent"))
	// Exercise opening an existing database without counters; installation/backfill
	// is serialized under the same immediate writer transaction as the schema.
	_, err = s.db.Exec("DROP TRIGGER mailbox_insert; DROP TRIGGER mailbox_raw_update; DROP TRIGGER mailbox_delete; DROP TABLE usage;")
	must(t, err)
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	check := func(records, payload int64) {
		t.Helper()
		var r, b int64
		must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&r, &b))
		if r != records || b != payload {
			t.Fatalf("counter drift: records=%d bytes=%d", r, b)
		}
	}
	check(2, int64(len(testRaw)))
	if _, err = s.Append(ctx, "Missing", bytes.NewReader(testRaw), false); err == nil {
		t.Fatal("append into absent folder accepted")
	}
	check(2, int64(len(testRaw)))
	_, err = s.Append(ctx, "Drafts", bytes.NewReader(testRaw), true)
	must(t, err)
	check(3, int64(2*len(testRaw)))
}
