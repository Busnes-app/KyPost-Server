package mailcache

import (
	"os"
	"testing"
)

// The decode memo trusts memory only while it matches the file: a write that
// fails after the in-memory change must not survive the next read.
func TestFailedWriteDoesNotOutliveTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert("INBOX", []Entry{{UID: 1, Sender: "a"}}); err != nil {
		t.Fatal(err)
	}
	mustSnapshot(t, s, "INBOX", 1)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err = s.Upsert("INBOX", []Entry{{UID: 2, Sender: "b"}})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("Upsert into a read-only directory succeeded")
	}
	if entries, _ := mustSnapshot(t, s, "INBOX", 5); len(entries) != 1 || entries[0].UID != 1 {
		t.Fatalf("served %+v, want only the persisted UID 1", entries)
	}
}
