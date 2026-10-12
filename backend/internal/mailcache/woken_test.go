package mailcache

import "testing"

// The inbox handler owns the snooze pin. The poller warms the same entries
// without it, and must not strip it, or every poll would flip the pin off and
// on again and phones would see the message change twice.
func TestUpsertKeepsWokenPin(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ov := Overview{UID: 4, Subject: "s", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z"}
	first, err := s.Sync("INBOX", 10, []Overview{ov}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ov.WokenAt = "2026-10-12T09:00:00Z"
	pinned, err := s.Sync("INBOX", 10, []Overview{ov}, first.Cursor)
	if err != nil || len(pinned.Updated) != 1 || pinned.Updated[0].WokenAt != ov.WokenAt {
		t.Fatalf("Sync with pin = %+v, %v; want one update carrying wokenAt", pinned, err)
	}

	if err := s.Upsert("INBOX", []Entry{{UID: 4, MessageID: "4", Subject: "s", Sender: "a@example.com", Status: "unread", AtUTC: "2026-01-01T00:00:00Z", Body: "b"}}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Sync("INBOX", 10, []Overview{ov}, pinned.Cursor)
	if err != nil || len(after.New)+len(after.Updated) != 0 {
		t.Fatalf("Sync after poller upsert = %+v, %v; want no change", after, err)
	}
	entries, _ := mustSnapshot(t, s, "INBOX", 1)
	if len(entries) != 1 || entries[0].WokenAt != ov.WokenAt {
		t.Fatalf("snapshot = %+v; want the pin kept", entries)
	}
}
