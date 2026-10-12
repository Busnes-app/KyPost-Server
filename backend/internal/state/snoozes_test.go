package state

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func newSnoozeStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSnoozeDueAndResnooze(t *testing.T) {
	s := newSnoozeStore(t)
	for id, at := range map[string]int64{"<a@x>": 300, "<b@x>": 100, "<c@x>": 900} {
		if err := s.Snooze(id, at); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.DueSnoozes(500)
	if err != nil || len(due) != 2 || due[0].MessageID != "<b@x>" || due[1].MessageID != "<a@x>" {
		t.Fatalf("DueSnoozes = %+v, %v; want b then a", due, err)
	}

	// A woken message snoozed again is snoozed, not pinned.
	if err := s.MarkWoken("<b@x>", "17", 500); err != nil {
		t.Fatal(err)
	}
	if err := s.Snooze("<b@x>", 2000); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.Woken(); len(w) != 0 {
		t.Fatalf("Woken after re-snooze = %+v, want none", w)
	}
	list, _ := s.ListSnoozes()
	if len(list) != 3 || list[2].MessageID != "<b@x>" || list[2].WakeAt != 2000 {
		t.Fatalf("ListSnoozes = %+v", list)
	}
}

func TestWokenPinsAndPrunes(t *testing.T) {
	s := newSnoozeStore(t)
	now := time.Now().Unix()
	old := now - (WokenPinDays+1)*24*3600
	for _, id := range []string{"<new@x>", "<old@x>", "<lost@x>"} {
		if err := s.Snooze(id, 1); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.MarkWoken("<new@x>", "5", now)
	_ = s.MarkWoken("<old@x>", "6", old)
	_ = s.MarkWoken("<lost@x>", "", now) // woken, but its INBOX id was not found

	w, err := s.Woken()
	if err != nil || len(w) != 2 || w[0].InboxID != "5" {
		t.Fatalf("Woken = %+v, %v; want new then old", w, err)
	}
	if due, _ := s.DueSnoozes(now); len(due) != 0 {
		t.Fatalf("woken rows must not be due again: %+v", due)
	}
	if err := s.Cleanup(30); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.Woken(); len(w) != 1 || w[0].MessageID != "<new@x>" {
		t.Fatalf("Woken after cleanup = %+v, want only new", w)
	}

	// New UIDs name other messages: the pin must go, not point at a stranger.
	if err := s.ResetUIDNamespace("0"); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.Woken(); len(w) != 0 {
		t.Fatalf("Woken after UID reset = %+v, want none", w)
	}
}

func TestSnoozeCap(t *testing.T) {
	s := newSnoozeStore(t)
	for i := range MaxSnoozes {
		if err := s.Snooze(fmt.Sprintf("<%d@x>", i), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Snooze("<over@x>", 1); !errors.Is(err, ErrTooManySnoozes) {
		t.Fatalf("Snooze over cap = %v, want ErrTooManySnoozes", err)
	}
	// Re-snoozing an existing row is not a new row.
	if err := s.Snooze("<0@x>", 5); err != nil {
		t.Fatalf("re-snooze at cap: %v", err)
	}
	if err := s.DropSnooze("<0@x>"); err != nil {
		t.Fatal(err)
	}
	if err := s.Snooze("<over@x>", 1); err != nil {
		t.Fatalf("Snooze after drop: %v", err)
	}
}
