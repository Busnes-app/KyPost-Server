package mailcache

import "testing"

func uids(rs []Removal) []int {
	out := []int{}
	for _, r := range rs {
		out = append(out, r.UID)
	}
	return out
}

// A message pushed below a full window by newer mail still exists. Reporting
// it as removed made the Android app delete it locally on every new arrival.
func TestSync_AgedOutIsNotReportedAsRemoved(t *testing.T) {
	s := newTestStore(t)
	first, _ := s.Sync("INBOX", 3, []Overview{ov(1, "a", "unread"), ov(2, "b", "unread"), ov(3, "c", "unread")}, 0)

	// 4 and 5 arrive and push 1 and 2 below the window.
	res, err := s.Sync("INBOX", 3, []Overview{ov(3, "c", "unread"), ov(4, "d", "unread"), ov(5, "e", "unread")}, first.Cursor)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := uids(res.AgedOut); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("agedOut = %v, want [1 2] (both below the oldest live UID 3)", got)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("removed = %v, want none", uids(res.Removed))
	}

	// A message missing from INSIDE the live range was deleted.
	res, err = s.Sync("INBOX", 3, []Overview{ov(3, "c", "unread"), ov(5, "e", "unread"), ov(6, "f", "unread")}, res.Cursor)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := uids(res.Removed); len(got) != 1 || got[0] != 4 || len(res.AgedOut) != 0 {
		t.Fatalf("removed = %v agedOut = %v, want removed [4]", got, uids(res.AgedOut))
	}

	// A window that is not full holds the whole mailbox: anything missing is gone.
	res, err = s.Sync("INBOX", 3, []Overview{ov(6, "f", "unread")}, res.Cursor)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := uids(res.Removed); len(got) != 2 || len(res.AgedOut) != 0 {
		t.Fatalf("removed = %v agedOut = %v, want 3 and 5 removed", got, uids(res.AgedOut))
	}
}

// More new mail than the window holds used to be silently skipped: the cursor
// moved past messages the client never received.
func TestSync_HasMoreWhenNewMailOverflowsTheWindow(t *testing.T) {
	s := newTestStore(t)
	first, _ := s.Sync("INBOX", 2, []Overview{ov(1, "a", "unread"), ov(2, "b", "unread")}, 0)
	if first.HasMore {
		t.Fatal("a since=0 snapshot has nothing to continue")
	}

	// One new message: the oldest live (2) is known, nothing was skipped.
	one, _ := s.Sync("INBOX", 2, []Overview{ov(2, "b", "unread"), ov(3, "c", "unread")}, first.Cursor)
	if one.HasMore {
		t.Fatal("hasMore with the oldest live message already known to the caller")
	}

	// Three new messages (4,5,6) into a window of 2: 4 is below the window.
	over, _ := s.Sync("INBOX", 2, []Overview{ov(5, "e", "unread"), ov(6, "f", "unread")}, one.Cursor)
	if !over.HasMore {
		t.Fatal("hasMore = false although new mail overflowed the window")
	}
}

// A cursor the window never issued must not be trusted as a delta base.
func TestSync_ForeignCursorResetsToFullWindow(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Sync("INBOX", 10, []Overview{ov(1, "a", "unread")}, 999)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.Reset || len(res.New) != 1 {
		t.Fatalf("future cursor: reset=%v new=%d, want a full window", res.Reset, len(res.New))
	}

	// A new window starts above every cursor the store has issued, so a cursor
	// from the shared window is below all of its revisions.
	old, _ := s.Sync("INBOX", 10, []Overview{ov(1, "a", "read"), ov(2, "b", "unread")}, 0)
	small, err := s.Sync(WindowKey("INBOX", 2), 2, []Overview{ov(1, "a", "unread"), ov(2, "b", "unread")}, old.Cursor)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(small.New) != 2 {
		t.Fatalf("new window reported %d new to an older foreign cursor, want 2", len(small.New))
	}
}

// Each limit has its own window: a 50-message poll between two 500-message
// polls no longer resets the larger window and re-sends everything as new.
func TestSync_LimitWindowsDoNotResetEachOther(t *testing.T) {
	s := newTestStore(t)
	live := []Overview{ov(1, "a", "unread"), ov(2, "b", "unread"), ov(3, "c", "unread")}
	big, _ := s.Sync("INBOX", 3, live, 0)
	if _, err := s.Sync(WindowKey("INBOX", 1), 1, live[2:], 0); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	again, err := s.Sync("INBOX", 3, live, big.Cursor)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(again.New)+len(again.Updated)+len(again.Removed)+len(again.AgedOut) != 0 || again.Reset {
		t.Fatalf("unchanged mailbox reported changes after another limit polled: %+v", again)
	}
}

func TestRemove_AppliesToEveryLimitWindow(t *testing.T) {
	s := newTestStore(t)
	live := []Overview{ov(1, "a", "unread")}
	s.Sync("INBOX", 10, live, 0)
	small, _ := s.Sync(WindowKey("INBOX", 5), 5, live, 0)
	if err := s.Remove("INBOX", 1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	res, _ := s.Sync(WindowKey("INBOX", 5), 5, []Overview{}, small.Cursor)
	if got := uids(res.Removed); len(got) != 1 || got[0] != 1 {
		t.Fatalf("removed in limit window = %v, want [1]", got)
	}
}

func TestWarmBody_SentLimitWindowStaysUncached(t *testing.T) {
	if warmBody(WindowKey("Sent", 50), Entry{Body: "secret"}) != "" {
		t.Fatal("a Sent body was cached under a limit window key")
	}
}

// A limit window reuses what the base window already warmed, so a phone at
// limit 50 does not re-fetch bodies the poller cached; a different message
// under the same UID inherits nothing.
func TestSync_LimitWindowInheritsBaseWarmth(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert("INBOX", []Entry{entry(1, "a", "unread", "warm body")}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	res, err := s.Sync(WindowKey("INBOX", 5), 5, []Overview{ov(1, "a", "read")}, 0)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.New) != 1 || res.New[0].Body != "warm body" || !res.New[0].PGPClassified || res.New[0].Status != "read" {
		t.Fatalf("limit window entry = %+v, want the base body with live flags", res.New)
	}

	other := ov(1, "a", "unread")
	other.Sender = "someone-else@example.com"
	res, _ = s.Sync(WindowKey("INBOX", 6), 6, []Overview{other}, 0)
	if len(res.New) != 1 || res.New[0].Body != "" {
		t.Fatalf("a different message under a reused UID inherited %+v", res.New)
	}
}
