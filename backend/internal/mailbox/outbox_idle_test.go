package mailbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func readIdle(t *testing.T, dir string) outboundIdle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, OutboundIdleFile))
	must(t, err)
	var m outboundIdle
	must(t, json.Unmarshal(raw, &m))
	return m
}

func retryAt(t *testing.T, s *Store) int64 {
	t.Helper()
	var at int64
	must(t, s.db.QueryRow("SELECT next_attempt FROM outbox_deliveries WHERE state='retryable'").Scan(&at))
	return at
}

func probe(t *testing.T, dir string) bool {
	t.Helper()
	pending, err := HasPendingOutbound(context.Background(), dir)
	must(t, err)
	return pending
}

// queueBehindMark commits due work without the writers' clear, as an older
// binary would, so only the mark can answer the next probe.
func queueBehindMark(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.db.Exec("INSERT INTO outbox(id,ciphertext,sent_bytes,sent_reserved) VALUES('raw',x'00',0,0)")
	must(t, err)
	_, err = s.db.Exec("INSERT INTO outbox_deliveries(job,sequence,state) VALUES('raw',0,'queued')")
	must(t, err)
}

func TestOutboundIdleMarkSkipsUntilAWriterClearsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	if probe(t, dir) {
		t.Fatal("empty mailbox has work")
	}
	m := readIdle(t, dir)
	if m.Until-m.Written != int64(outboundIdleMax/time.Second) {
		t.Fatal("mark without retries is not bounded by outboundIdleMax", m)
	}
	queueBehindMark(t, s)
	if probe(t, dir) {
		t.Fatal("valid mark did not skip the query")
	}
	must(t, s.QueueOutbound(context.Background(), outboxMaster, outboxTestID, outboxTestJob()))
	if !probe(t, dir) {
		t.Fatal("enqueue did not clear the mark")
	}
	if _, err := os.Stat(filepath.Join(dir, OutboundIdleFile)); !os.IsNotExist(err) {
		t.Fatal("pending mailbox was marked idle", err)
	}
}

func TestOutboundIdleMarkNotWrittenDuringAWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	release, err := s.clearOutboundIdle()
	must(t, err)
	if probe(t, dir) {
		t.Fatal("empty mailbox has work")
	}
	release()
	if _, err = os.Stat(filepath.Join(dir, OutboundIdleFile)); !os.IsNotExist(err) {
		t.Fatal("probe marked a mailbox with a writer mid-transaction", err)
	}
}

func TestOutboundIdleMarkRefusedWhenStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	probe(t, dir)
	good := readIdle(t, dir)
	info, err := os.Stat(filepath.Join(dir, "mailbox.db"))
	must(t, err)
	now := time.Now()
	for name, m := range map[string]outboundIdle{
		"other inode":      {good.Dev, good.Ino + 1, good.Written, good.Until},
		"expired":          {good.Dev, good.Ino, now.Add(-time.Hour).Unix(), now.Add(-time.Second).Unix()},
		"written later":    {good.Dev, good.Ino, now.Add(time.Hour).Unix(), now.Add(time.Hour + time.Minute).Unix()},
		"beyond the bound": {good.Dev, good.Ino, now.Unix(), now.Add(outboundIdleMax + time.Minute).Unix()},
	} {
		raw, _ := json.Marshal(m)
		must(t, os.WriteFile(filepath.Join(dir, OutboundIdleFile), raw, 0o600))
		if idleMarked(dir, info, now) {
			t.Error(name, "mark accepted")
		}
	}
	queueBehindMark(t, s)
	if !probe(t, dir) {
		t.Fatal("stale mark hid due work")
	}
}

func TestOutboundIdleMarkFollowsTheDatabaseFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	probe(t, dir)
	queueBehindMark(t, s)
	must(t, s.Close())
	// A restore or re-preparation publishes a new file under the same name.
	path := filepath.Join(dir, "mailbox.db")
	raw, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path+".new", raw, 0o600))
	must(t, os.Rename(path+".new", path))
	if st, _ := os.Stat(path); st.Sys().(*syscall.Stat_t).Ino == readIdle(t, dir).Ino {
		t.Skip("filesystem reused the inode")
	}
	if !probe(t, dir) {
		t.Fatal("mark for the replaced file hid due work")
	}
}
