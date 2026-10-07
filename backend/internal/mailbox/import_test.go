package mailbox

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, data []byte, limit, maxExpanded int64) (msgs []string, skipped int, err error) {
	t.Helper()
	return readCapped(t, data, limit, maxExpanded, 1<<20)
}

func readCapped(t *testing.T, data []byte, limit, maxExpanded int64, maxMessages int) (msgs []string, skipped int, err error) {
	t.Helper()
	skipped, err = ReadImport(bytes.NewReader(data), int64(len(data)), limit, maxExpanded, maxMessages, func(raw []byte) error {
		msgs = append(msgs, string(raw))
		return nil
	})
	return msgs, skipped, err
}

func TestReadImportMbox(t *testing.T) {
	// mboxrd/mboxo quoting, CRLF and LF messages, and an unquoted "From "
	// inside a paragraph, which only splits after a blank line.
	file := "From a@example.test Tue Sep  1 10:00:00 2026\r\n" +
		"Subject: one\r\n\r\n>From quoted\r\n>>From twice\r\nbody\r\nFrom inside a paragraph\r\n\r\n" +
		"From b@example.test Tue Sep  1 10:00:00 2026\n" +
		"Subject: two\n\nlf only\n\n" +
		"From MAILER-DAEMON Thu Jan  1 00:00:00 1970\n" +
		"Subject: last\n\nno trailing blank\n"
	msgs, skipped, err := readAll(t, []byte(file), 1<<20, 0)
	want := []string{
		"Subject: one\r\n\r\nFrom quoted\r\n>From twice\r\nbody\r\nFrom inside a paragraph\r\n",
		"Subject: two\n\nlf only\n",
		"Subject: last\n\nno trailing blank\n",
	}
	if err != nil || skipped != 0 || fmt.Sprintf("%q", msgs) != fmt.Sprintf("%q", want) {
		t.Fatalf("mbox: %q skipped=%d err=%v", msgs, skipped, err)
	}

	// Our own export round-trips exactly.
	var out bytes.Buffer
	in := [][]byte{testRaw, []byte("Subject: q\r\n\r\nFrom x\r\n>From y\r\n\r\nFrom z\r\n")}
	for i, raw := range in {
		if err = WriteMboxrd(&out, ExportMessage{ID: int64(i), Raw: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if msgs, skipped, err = readAll(t, out.Bytes(), 1<<20, 0); err != nil || skipped != 0 || len(msgs) != 2 || msgs[0] != string(in[0]) || msgs[1] != string(in[1]) {
		t.Fatalf("round trip %q %d %v", msgs, skipped, err)
	}

	// Per-message cap: one over the limit is skipped, its neighbours kept,
	// and a message exactly at the limit passes.
	exact := "Subject: x\r\n\r\n" + strings.Repeat("a", 100-16) + "\r\n"
	big := "Subject: y\r\n\r\n" + strings.Repeat("b", 200) + "\r\n"
	file = "From a b\r\n" + exact + "\r\nFrom a b\r\n" + big + "\r\nFrom a b\r\nSubject: z\r\n\r\nz\r\n"
	if msgs, skipped, err = readAll(t, []byte(file), 100, 0); err != nil || skipped != 1 || len(msgs) != 2 || msgs[0] != exact {
		t.Fatalf("cap %q %d %v", msgs, skipped, err)
	}
	// A line longer than the read buffer is still one line.
	long := "Subject: long\n\n" + strings.Repeat("c", 200<<10) + "\nFrom not a separator\n"
	if msgs, _, err = readAll(t, []byte("From a b\n"+long), 1<<20, 0); err != nil || len(msgs) != 1 || msgs[0] != long {
		t.Fatal("long line", len(msgs), err)
	}
}

// Junk never reaches the store callback, and a flood of it or of valid
// duplicates stops at the message cap.
func TestReadImportFloods(t *testing.T) {
	junk := bytes.Repeat([]byte("From \n\n"), 200000)
	start := time.Now()
	msgs, skipped, err := readCapped(t, junk, 1<<20, 0, 1000)
	if !errors.Is(err, ErrImportTooMany) || len(msgs) != 0 || skipped != 1000 || time.Since(start) > 5*time.Second {
		t.Fatal("empty flood", len(msgs), skipped, err, time.Since(start))
	}
	if msgs, skipped, err = readCapped(t, []byte("From \n\nFrom a\nno header\n\nFrom b\nSubject: ok\n\nx\n"), 1<<20, 0, 10); err != nil || skipped != 2 || len(msgs) != 1 {
		t.Fatal("junk reached the callback", msgs, skipped, err)
	}
	dupes := bytes.Repeat([]byte("From a b\nSubject: same\n\nx\n\n"), 5000)
	if msgs, _, err = readCapped(t, dupes, 1<<20, 0, 1000); !errors.Is(err, ErrImportTooMany) || len(msgs) != 1000 {
		t.Fatal("duplicate flood", len(msgs), err)
	}
	many := map[string][]byte{}
	order := []string{}
	for i := range 20 {
		name := fmt.Sprintf("%d.eml", i)
		many[name], order = nil, append(order, name)
	}
	if _, skipped, err = readCapped(t, zipOf(t, many, order...), 1<<20, 1<<20, 10); !errors.Is(err, ErrImportTooMany) || skipped != 10 {
		t.Fatal("zip flood", skipped, err)
	}
}

func TestReadImportEML(t *testing.T) {
	if msgs, skipped, err := readAll(t, testRaw, 1<<20, 0); err != nil || skipped != 0 || len(msgs) != 1 || msgs[0] != string(testRaw) {
		t.Fatal("eml", msgs, skipped, err)
	}
	if msgs, skipped, err := readAll(t, testRaw, 10, 0); err != nil || skipped != 1 || len(msgs) != 0 {
		t.Fatal("oversized eml", msgs, skipped, err)
	}
}

// noise is hex of pseudo-random bytes: mail-like, compressing about 2:1.
func noise(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	raw := make([]byte, n/2)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return []byte(hex.EncodeToString(raw))
}

func zipOf(t *testing.T, entries map[string][]byte, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, name := range order {
		w, err := z.Create(name)
		must(t, err)
		_, err = w.Write(entries[name])
		must(t, err)
	}
	must(t, z.Close())
	return buf.Bytes()
}

func TestReadImportZip(t *testing.T) {
	// Entry names are never paths: climbing, absolute and drive names are
	// read like any other, and nothing is written to disk.
	work := t.TempDir()
	t.Chdir(filepath.Join(work))
	names := []string{"INBOX/", "INBOX/1.eml", "../../evil.eml", "/abs.EML", `C:\x.eml`, "notes.txt", "bomb.eml", "big.eml"}
	entries := map[string][]byte{
		"INBOX/":         nil,
		"INBOX/1.eml":    testRaw,
		"../../evil.eml": []byte("Subject: evil\r\n\r\nx\r\n"),
		"/abs.EML":       []byte("Subject: abs\r\n\r\nx\r\n"),
		`C:\x.eml`:       []byte("Subject: drive\r\n\r\nx\r\n"),
		"notes.txt":      []byte("Subject: not mail\r\n\r\n"),
		// A valid message, so only the ratio guard can refuse it.
		"bomb.eml": append([]byte("Subject: bomb\r\n\r\n"), bytes.Repeat([]byte("a"), 4<<20)...),
		"big.eml":  append([]byte("Subject: big\r\n\r\n"), noise(1536<<10)...),
	}
	data := zipOf(t, entries, names...)
	msgs, skipped, err := readAll(t, data, 25<<20, 64<<20)
	if err != nil || skipped != 2 || len(msgs) != 5 || msgs[0] != string(testRaw) || !strings.HasPrefix(msgs[4], "Subject: big") {
		t.Fatalf("zip: %d messages, skipped=%d err=%v", len(msgs), skipped, err)
	}
	if left, _ := os.ReadDir(work); len(left) != 0 {
		t.Fatal("zip wrote to disk", left)
	}
	if _, err = os.Stat(filepath.Join(work, "..", "..", "evil.eml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("zip escaped", err)
	}

	// The bomb is over the ratio guard; with a tight message limit the big
	// entry is skipped as oversized too.
	if _, skipped, err = readAll(t, zipOf(t, entries, "bomb.eml", "big.eml", "INBOX/1.eml"), 1<<20, 64<<20); err != nil || skipped != 2 {
		t.Fatal("bomb and oversized", skipped, err)
	}
	// Too many expanded bytes refuse the archive.
	if _, _, err = readAll(t, zipOf(t, entries, "big.eml", "INBOX/1.eml"), 25<<20, 1<<20); !errors.Is(err, ErrImportArchive) {
		t.Fatal("expansion cap", err)
	}
	// Too many entries refuse the archive before any message is read.
	many := map[string][]byte{}
	order := []string{}
	for i := range maxImportZipEntries + 1 {
		name := fmt.Sprintf("%d.eml", i)
		many[name], order = testRaw, append(order, name)
	}
	if msgs, _, err = readAll(t, zipOf(t, many, order...), 25<<20, 1<<30); !errors.Is(err, ErrImportArchive) || len(msgs) != 0 {
		t.Fatal("entry cap", len(msgs), err)
	}
	if _, _, err = readAll(t, []byte("PK\x03\x04 not a zip"), 25<<20, 1<<30); !errors.Is(err, ErrImportArchive) {
		t.Fatal("corrupt zip", err)
	}
}

func TestNativeImportMessage(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	folder, err := c.ImportFolder(ctx, "", true)
	if err != nil || folder != "Imported" {
		t.Fatal("default folder", folder, err)
	}
	if folder, err = c.ImportFolder(ctx, "inbox", true); err != nil || folder != "INBOX" {
		t.Fatal("inbox folder", folder, err)
	}
	for _, create := range []bool{false, true} {
		if _, err = c.ImportFolder(ctx, "Missing/Child", create); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing parent", create, err)
		}
		if _, err = c.ImportFolder(ctx, "../x", create); err == nil {
			t.Fatal("unsafe folder accepted", create)
		}
	}
	// Checking a new folder does not create it.
	if folder, err = c.ImportFolder(ctx, "Imported/2026", false); err != nil || folder != "Imported/2026" {
		t.Fatal("nested folder check", folder, err)
	}
	if all, _ := s.Folders(ctx); strings.Contains(strings.Join(all, ","), "2026") {
		t.Fatal("check created the folder", all)
	}
	if folder, err = c.ImportFolder(ctx, "Imported/2026", true); err != nil || folder != "Imported/2026" {
		t.Fatal("nested folder", folder, err)
	}

	// Stored seen, without a receipt; the same bytes again are a duplicate in
	// that folder only.
	must(t, c.ImportMessage(ctx, "INBOX", testRaw))
	if err = c.ImportMessage(ctx, "INBOX", testRaw); !errors.Is(err, ErrDuplicate) {
		t.Fatal("re-import duplicated", err)
	}
	must(t, c.ImportMessage(ctx, "Imported", testRaw))
	for _, bad := range [][]byte{nil, []byte("no header line\r\n"), append([]byte("Subject: big\r\n\r\n"), make([]byte, testLimits.MessageBytes)...)} {
		if err = c.ImportMessage(ctx, "Imported", bad); !errors.Is(err, ErrUnimportable) {
			t.Fatalf("unimportable %.20q: %v", bad, err)
		}
	}
	var receipts, unseen int
	must(t, s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	must(t, s.db.QueryRow("SELECT count(*) FROM messages WHERE seen=0").Scan(&unseen))
	if receipts != 0 || unseen != 0 {
		t.Fatal("import wrote receipts or unseen mail", receipts, unseen)
	}
	// The poller's only source (rules, sorter, notifications and incoming
	// encryption) never sees imported mail, even in INBOX.
	if msgs, _, err := c.ListUnreadInbox(ctx, ""); err != nil || len(msgs) != 0 {
		t.Fatal("imported mail reached the poller", len(msgs), err)
	}
	// Exact bytes.
	list, err := s.List(ctx, "INBOX", 0, 10)
	must(t, err)
	// Marked unread later, imported mail still never reaches the poller.
	must(t, s.Update(ctx, "INBOX", list[0].ID, false, false, nil))
	if msgs, _, err := c.ListUnreadInbox(ctx, ""); err != nil || len(msgs) != 0 {
		t.Fatal("unread imported mail reached the poller", len(msgs), err)
	}
	importClient(t, s, "received", []byte("Subject: received\r\n\r\nx\r\n"))
	if msgs, _, err := c.ListUnreadInbox(ctx, ""); err != nil || len(msgs) != 1 {
		t.Fatal("received mail no longer reaches the poller", len(msgs), err)
	}
	if raw, err := s.Raw(ctx, "INBOX", list[0].ID); err != nil || !bytes.Equal(raw, testRaw) {
		t.Fatal("bytes changed", err)
	}
	// A deleted copy no longer blocks a re-import.
	must(t, s.Delete(ctx, "INBOX", list[0].ID))
	must(t, c.ImportMessage(ctx, "INBOX", testRaw))
}

func TestNativeImportCapacity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, Limits{MessageBytes: 1 << 10, PayloadBytes: 1 << 10, Records: 10})
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	must(t, c.ImportMessage(ctx, "INBOX", testRaw))
	big := append([]byte("Subject: fills\r\n\r\n"), bytes.Repeat([]byte("x"), 1000-18)...)
	if err = c.ImportMessage(ctx, "INBOX", big); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity", err)
	}
}
