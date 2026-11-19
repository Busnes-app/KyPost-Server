package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

var threadDay0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// member builds a threadMember on day d with the given header lines.
func member(uid int, subject string, d int, lines ...string) threadMember {
	ov := imapadapter.Overview{UID: uid, MessageID: strconv.Itoa(uid), Subject: subject, AtUTC: threadDay0.AddDate(0, 0, d).Format(time.RFC3339)}
	return newThreadMember("INBOX", ov, lines, true)
}

// groupFor runs the grouping with a live context.
func groupFor(t *testing.T, members []threadMember) [][]threadMember {
	t.Helper()
	groups, err := groupThreads(context.Background(), members)
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func groupedUIDs(groups [][]threadMember) [][]int {
	out := [][]int{}
	for _, g := range groups {
		uids := []int{}
		for _, m := range g {
			uids = append(uids, m.ov.UID)
		}
		out = append(out, uids)
	}
	slices.SortFunc(out, func(a, b []int) int { return a[0] - b[0] })
	return out
}

func TestGroupThreads(t *testing.T) {
	unknown := member(15, "Re: Report", 3)
	unknown.known = false
	got := groupedUIDs(groupFor(t, []threadMember{
		// A header chain, listed newest first, comes back oldest first.
		member(3, "Re: Re: Plan", 2, "References: <a@x> <b@x>", "Message-ID: <c@x>"),
		member(1, "Plan", 0, "Message-ID: <a@x>"),
		member(2, "Re: Plan", 1, "Message-ID: <b@x>", "In-Reply-To: <a@x>"),
		// Siblings whose parent is outside the window still meet.
		member(4, "Re: Lunch", 0, "In-Reply-To: <gone@x>"),
		member(5, "Re: Lunch", 1, "References: <gone@x>"),
		// Two unrelated invoices never merge on their subject.
		member(6, "Invoice", 0, "Message-ID: <i1@x>"),
		member(7, "Invoice", 1, "Message-ID: <i2@x>"),
		// A threaded reply to the first joins it by header, not the newer one by subject.
		member(8, "Re: Invoice", 2, "Message-ID: <i3@x>", "In-Reply-To: <i1@x>"),
		// Nor do two invoices without any headers.
		member(9, "Invoice", 0),
		member(10, "Invoice", 1),
		// A reply with no threading headers joins the newest earlier same subject.
		member(11, "Report", 0, "Message-ID: <r1@x>"),
		member(12, "Report", 2, "Message-ID: <r2@x>"),
		member(13, "AW: report", 3),
		// Not past the window.
		member(14, "Re: Old news", 30),
		member(16, "Old news", 0, "Message-ID: <o@x>"),
		// A reply whose headers were not fetched is unknown, not headerless.
		unknown,
	}))
	want := [][]int{{1, 2, 3}, {4, 5}, {6, 8}, {7}, {9}, {10}, {11}, {12, 13}, {14}, {15}, {16}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("groups %v, want %v", got, want)
	}
}

// One side of the chain without a Message-ID still joins a threaded reply.
func TestGroupThreadsOriginalWithoutMessageID(t *testing.T) {
	got := groupedUIDs(groupFor(t, []threadMember{
		member(1, "Quote", 0),
		member(2, "Re: Quote", 1, "In-Reply-To: <missing@x>"),
	}))
	if !slices.EqualFunc(got, [][]int{{1, 2}}, slices.Equal) {
		t.Fatalf("groups %v", got)
	}
}

func getConversations(t *testing.T, srv *Server, userID, query string) []conversationOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/inbox/conversations"+query, nil)
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Conversations []conversationOut `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Conversations
}

// IMAP: the grouping reads header fields and never a body.
func TestInboxConversationsReadsHeadersOnly(t *testing.T) {
	fake := &fakeMailClient{
		overviews: []imapadapter.Overview{
			{UID: 2, MessageID: "2", Subject: "Re: Plan", AtUTC: threadDay0.Add(time.Hour).Format(time.RFC3339)},
			{UID: 1, MessageID: "1", Subject: "Plan", AtUTC: threadDay0.Format(time.RFC3339)},
		},
		headerLines: map[int][]string{1: {"Message-ID: <a@x>"}, 2: {"In-Reply-To: <a@x>"}},
	}
	srv, userID := mailBodyServer(t, fake)
	got := getConversations(t, srv, userID, "")
	if fake.bodiesCalls != 0 || fake.unreadCalls != 0 || !slices.Equal(fake.headerFields[:3], threadHeaderFields) {
		t.Fatalf("bodies %d, unread %d, headers %v", fake.bodiesCalls, fake.unreadCalls, fake.headerFields)
	}
	if len(got) != 1 || !slices.Equal(got[0].MessageIDs, []string{"1", "2"}) || got[0].ID != "1" {
		t.Fatalf("conversations %+v", got)
	}
}

// Native: replies stay in Sent and appear inline; unrelated mail stays apart.
func TestInboxConversationsNativeIncludesSent(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	srv, owner := mailBodyServer(t, &fakeMailClient{})
	store, err := mailbox.Open(filepath.Join(t.TempDir(), "mailbox"), mailbox.Owner{Issuer: "https://identity.example.test", Subject: owner, Mailbox: owner}, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client, err := mailbox.NewClient(store, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	nativeState, err := state.NewNative(filepath.Join(t.TempDir(), "native-state"), client.MailSourceIdentity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nativeState.Close() })
	srv.userMu.Lock()
	srv.userStores[owner] = nativeState
	srv.userMail[owner] = &serverMailEntry{client: client, updatedAt: "test"}
	srv.userMu.Unlock()

	msg := func(subject, date string, headers ...string) []byte {
		return []byte(strings.Join(append([]string{"From: friend@outside.test", "To: me@example.com", "Subject: " + subject, "Date: " + date}, headers...), "\r\n") + "\r\n\r\nbody\r\n")
	}
	seq := 0
	deliver := func(raw []byte) string {
		t.Helper()
		seq++
		id, err := store.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: strconv.Itoa(seq), Sender: "friend@outside.test", Recipients: []mailbox.Recipient{{Address: "me@example.com", Generation: 1}}}, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return "n1:" + store.MessageReferenceGeneration() + ":" + strconv.FormatInt(id, 10)
	}
	original := deliver(msg("Plan", "Mon, 02 Nov 2026 09:00:00 +0000", "Message-ID: <plan@outside.test>"))
	deliver(msg("Invoice", "Mon, 02 Nov 2026 10:00:00 +0000", "Message-ID: <inv1@outside.test>"))
	deliver(msg("Invoice", "Tue, 03 Nov 2026 10:00:00 +0000", "Message-ID: <inv2@outside.test>"))
	sentID, err := store.Append(ctx, "Sent", bytes.NewReader(msg("Re: Plan", "Mon, 02 Nov 2026 11:00:00 +0000", "Message-ID: <mine@example.com>", "In-Reply-To: <plan@outside.test>")), true)
	if err != nil {
		t.Fatal(err)
	}
	answer := deliver(msg("Re: Plan", "Mon, 02 Nov 2026 12:00:00 +0000", "Message-ID: <again@outside.test>", "References: <plan@outside.test> <mine@example.com>"))

	got := getConversations(t, srv, owner, "?mailbox=INBOX")
	if len(got) != 1 {
		t.Fatalf("conversations %+v, want one", got)
	}
	c := got[0]
	sentRef := "n1:" + store.MessageReferenceGeneration() + ":" + strconv.FormatInt(sentID, 10)
	if c.ID != original || !slices.Equal(c.MessageIDs, []string{original, answer}) || len(c.Sent) != 1 || c.Sent[0].MessageID != sentRef || c.Sent[0].Mailbox != "Sent" {
		t.Fatalf("conversation %+v", c)
	}
	// Grouping moves nothing: the reply is still listed in Sent.
	if sent := getConversations(t, srv, owner, "?mailbox=Sent"); len(sent) != 0 {
		t.Fatalf("Sent alone has no conversations, got %+v", sent)
	}
}

func TestMailSettingsConversationsSwitch(t *testing.T) {
	srv, userID := mailBodyServer(t, &fakeMailClient{})
	call := func(method, body string) (int, string) {
		req := httptest.NewRequest(method, "/api/settings/mail", strings.NewReader(body))
		authRequestAs(srv, req, userID)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	if code, body := call(http.MethodGet, ""); code != 200 || body != `{"conversations":true}` {
		t.Fatalf("default %d %s, want on", code, body)
	}
	if code, body := call(http.MethodPut, `{"conversations":false}`); code != 200 || body != `{"conversations":false}` {
		t.Fatalf("turn off %d %s", code, body)
	}
	if code, body := call(http.MethodGet, ""); code != 200 || body != `{"conversations":false}` {
		t.Fatalf("stored %d %s", code, body)
	}
	for _, bad := range []string{`{}`, `{"conversations":"no"}`, `{"conversations":true,"x":1}`} {
		if code, _ := call(http.MethodPut, bad); code != 400 {
			t.Fatalf("%s answered %d, want 400", bad, code)
		}
	}
}

// longSubjectWindow is 500 replies whose subjects carry `prefixes` reply prefixes
// each, all with the same base subject and no threading headers: the worst
// case for the subject fallback.
func longSubjectWindow(prefixes int) []imapadapter.Overview {
	subject := strings.Repeat("Re: AW: ", prefixes/2) + "Quarterly numbers"
	out := make([]imapadapter.Overview, 500)
	for i := range out {
		out[i] = imapadapter.Overview{UID: i + 1, MessageID: strconv.Itoa(i + 1), Subject: subject, AtUTC: threadDay0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}
	}
	return out
}

func groupWindow(overviews []imapadapter.Overview) error {
	members := make([]threadMember, len(overviews))
	for i, ov := range overviews {
		members[i] = newThreadMember("INBOX", ov, nil, true)
	}
	_, err := groupThreads(context.Background(), members)
	return err
}

func BenchmarkGroupThreadsLongSubjects(b *testing.B) {
	window := longSubjectWindow(2000)
	b.ReportAllocs()
	for b.Loop() {
		if err := groupWindow(window); err != nil {
			b.Fatal(err)
		}
	}
}

// Normalising a sender's subject costs memory linear in its length: doubling
// every subject's prefix chain must not quadruple the bytes allocated.
func TestGroupThreadsAllocationsLinearInSubjectBytes(t *testing.T) {
	allocated := func(prefixes int) uint64 {
		window := longSubjectWindow(prefixes)
		if err := groupWindow(window); err != nil { // warm caches outside the measurement
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := groupWindow(window); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	n, twice := allocated(1000), allocated(2000)
	if twice > 3*n {
		t.Fatalf("allocated %d bytes at N and %d at 2N: superlinear in subject length", n, twice)
	}
}

func TestGroupThreadsStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := groupThreads(ctx, []threadMember{member(1, "Re: Plan", 1)}); err == nil {
		t.Fatal("grouping ran on after its request was cancelled")
	}
}
