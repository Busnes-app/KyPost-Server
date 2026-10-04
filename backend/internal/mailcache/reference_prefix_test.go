package mailcache

import (
	"strings"
	"testing"
)

func TestNativeReferencePrefixDropsWindowsPreservesPolicy(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	source := "native:" + strings.Repeat("1", 64)
	if err := s.BindMailSource(source); err != nil {
		t.Fatal(err)
	}
	first, second := "n1:11111111-1111-4111-8111-111111111111:", "n1:22222222-2222-4222-8222-222222222222:"
	if err := s.BindMessageReferencePrefix(first); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Sent", "INBOX/Work"} {
		if err := s.Upsert(folder, []Entry{{UID: 1, MessageID: "1", Body: "old body", PGPSigned: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.OmitBodies(); err != nil {
		t.Fatal(err)
	}
	other, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.BindMessageReferencePrefix(first); err != nil {
		t.Fatal(err)
	}
	if entries, _, err := other.Snapshot("INBOX", 10); err != nil || len(entries) != 1 {
		t.Fatal("same generation discarded window", entries, err)
	}
	if err := other.BindMessageReferencePrefix(second); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Sent", "INBOX/Work"} {
		if entries, _, err := s.Snapshot(folder, 10); err != nil || len(entries) != 0 {
			t.Fatal("old generation window survived", folder, entries, err)
		}
	}
	if err := s.Upsert("INBOX", []Entry{{UID: 1, MessageID: "1", Body: "new body"}}); err != nil {
		t.Fatal(err)
	}
	if err := other.BindMessageReferencePrefix(second); err != nil {
		t.Fatal(err)
	}
	entries, _, err := other.Snapshot("INBOX", 10)
	if err != nil || len(entries) != 1 || entries[0].Body != "" {
		t.Fatal("writer lost prefix/body omission policy", entries, err)
	}
	if err := other.CheckMailSource(source); err != nil {
		t.Fatal("immutable source changed", err)
	}
	if err := other.BindMessageReferencePrefix(""); err == nil {
		t.Fatal("empty native capability accepted")
	}
}
