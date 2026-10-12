package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestParseWakeAt(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for raw, ok := range map[string]bool{
		"2026-10-12T10:00:00Z":      true,
		"2026-10-12T11:00:00+02:00": false, // 09:00Z: not in the future
		"2026-10-12T08:59:00Z":      false,
		"2027-10-13T10:00:00Z":      false, // past a year
		"tomorrow":                  false,
		"":                          false,
	} {
		if _, err := parseWakeAt(raw, now); (err == nil) != ok {
			t.Errorf("parseWakeAt(%q) err=%v, want ok=%v", raw, err, ok)
		}
	}
}

type failingMoveClient struct{ *fakeMailClient }

func (failingMoveClient) ApplyInboxAction(context.Context, string, string, string, string) error {
	return errors.New("move refused")
}

func TestSnoozeMessage(t *testing.T) {
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	fake := &fakeMailClient{headerLines: map[int][]string{7: {"Message-ID: <m7@example.com>"}}}

	if err := snoozeMessage(ctx, fake, st, "7", "", 100); err != nil {
		t.Fatalf("snoozeMessage: %v", err)
	}
	if rows, _ := st.ListSnoozes(); len(rows) != 1 || rows[0].MessageID != "<m7@example.com>" {
		t.Fatalf("ListSnoozes = %+v", rows)
	}

	// A failed move must not leave a row that would later "wake" a message
	// that never left the inbox.
	if err := snoozeMessage(ctx, failingMoveClient{fake}, st, "7", "", 200); err == nil {
		t.Fatal("failed move reported success")
	}
	if rows, _ := st.ListSnoozes(); len(rows) != 0 {
		t.Fatalf("row kept after failed move: %+v", rows)
	}

	fake.headerLines = map[int][]string{7: {}}
	if err := snoozeMessage(ctx, fake, st, "7", "", 100); !errors.Is(err, imapadapter.ErrNoMessageID) {
		t.Fatalf("no Message-ID: err=%v", err)
	}
}

// Phones read the inbox through cursor deltas, so the pin has to arrive as a
// change to a message they already hold, and leave the same way.
func TestServeInboxCarriesWokenPinThroughDeltas(t *testing.T) {
	srv := newTestServer(t)
	all, _ := srv.users.List()
	userID := all[0].ID
	st, err := srv.userStore(userID)
	if err != nil {
		t.Fatal(err)
	}
	cache := testInboxCache(t)
	fake := &fakeMailClient{bodies: map[int]string{}, overviews: overviewsFor(1, 2, 3)}
	poll := func(since int64) deltaWire {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.serveInbox(rec, context.Background(), userID, fake, cache, config.Default(), "", maxInboxLimit, since, true, false, false)
		var out deltaWire
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		return out
	}
	rows := func(w deltaWire) []inboxEmail {
		var out []inboxEmail
		for _, r := range w.ByTab {
			out = append(out, r...)
		}
		return out
	}

	first := poll(0)
	woke := time.Now().Unix()
	for _, id := range []string{"<two@x>", "<gone@x>"} {
		if err := st.Snooze(id, 1); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.MarkWoken("<two@x>", "2", woke)
	_ = st.MarkWoken("<gone@x>", "9", woke) // not in a non-full window: left INBOX

	pinned := poll(first.Cursor)
	got := rows(pinned)
	if len(got) != 1 || got[0].MessageID != "2" || got[0].ChangeType != "updated" || got[0].WokenAt != time.Unix(woke, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("delta after wake = %+v; want message 2 updated with wokenAt", got)
	}
	if w, _ := st.Woken(); len(w) != 1 || w[0].MessageID != "<two@x>" {
		t.Fatalf("Woken = %+v; the pin for a message gone from INBOX must be dropped", w)
	}

	if quiet := poll(pinned.Cursor); len(rows(quiet)) != 0 {
		t.Fatalf("unchanged pin re-sent: %+v", rows(quiet))
	}

	if err := st.DropSnooze("<two@x>"); err != nil {
		t.Fatal(err)
	}
	unpinned := rows(poll(pinned.Cursor))
	if len(unpinned) != 1 || unpinned[0].MessageID != "2" || unpinned[0].WokenAt != "" {
		t.Fatalf("delta after unpin = %+v; want message 2 updated without wokenAt", unpinned)
	}
}
