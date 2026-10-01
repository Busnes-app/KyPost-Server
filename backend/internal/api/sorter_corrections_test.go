package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// sorterFixture: a user with labels (Updates also produces the $Notice keyword),
// a cached INBOX message 7, and a recorded prediction that it is Updates.
func sorterFixture(t *testing.T) (*Server, users.User, *state.Store, mailcache.Entry) {
	t.Helper()
	srv, u := newTestServerWithUser(t)
	settings := config.DefaultUserSettings()
	settings.Labels.Seeded = true
	settings.Labels.Allowlist = []string{"Primary", "Promotions", "Updates"}
	settings.Labels.KeywordMappings = map[string][]string{"Updates": {"$Notice"}}
	if err := config.SaveUserSettings(srv.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	e := mailcache.Entry{UID: 7, MessageID: "7", Sender: "Shop <news@shop.example>", Subject: "Spring sale", Keywords: []string{"Updates"}}
	cache, err := srv.userMailCacheStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Upsert(inboxCacheMailboxKey(""), []mailcache.Entry{e}); err != nil {
		t.Fatal(err)
	}
	store, err := srv.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSorterPrediction("7", state.SorterCheck(e.Sender, e.Subject), "m", []float32{1, 0}, "Updates"); err != nil {
		t.Fatal(err)
	}
	return srv, u, store, e
}

func corrections(t *testing.T, store *state.Store) []state.SorterExample {
	t.Helper()
	ex, err := store.SorterCorrectionsStrict("m")
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func TestLabelForKeyword(t *testing.T) {
	labels := config.UserLabelSettings{Allowlist: []string{"Primary", "Updates"},
		KeywordMappings: map[string][]string{"Updates": {"$Notice"}}}
	for keyword, want := range map[string]string{"updates": "Updates", "$notice": "Updates", "Primary": "Primary", "Flagged": ""} {
		if got := labelForKeyword(keyword, labels); got != want {
			t.Errorf("labelForKeyword(%q) = %q, want %q", keyword, got, want)
		}
	}
}

// Labelling through the reader is a correction when it differs from what the
// sorter filed; a keyword that is not an account label teaches nothing.
func TestLearnFromLabelAction(t *testing.T) {
	srv, u, store, _ := sorterFixture(t)
	srv.learnFromLabelAction(u.ID, "Flagged", []string{"7"})
	if n := len(corrections(t, store)); n != 0 {
		t.Fatalf("non-label keyword recorded %d corrections", n)
	}
	srv.learnFromLabelAction(u.ID, "promotions", []string{"7", "999"})
	if ex := corrections(t, store); len(ex) != 1 || ex[0].Label != "Promotions" {
		t.Fatalf("corrections = %+v, want one Promotions", ex)
	}
}

// A keyword change made in another client is seen by the inbox sync. Exactly
// one other label is a correction; several at once is ambiguous; returning to
// the sorter's own label withdraws the correction.
func TestLearnFromSyncedKeywords(t *testing.T) {
	srv, u, store, e := sorterFixture(t)
	inbox := inboxCacheMailboxKey("")

	ambiguous := e
	ambiguous.Keywords = []string{"Updates", "Primary", "Promotions"}
	srv.learnFromSyncedKeywords(u.ID, inbox, []mailcache.Entry{ambiguous})
	if n := len(corrections(t, store)); n != 0 {
		t.Fatalf("ambiguous relabel recorded %d corrections", n)
	}

	moved := e
	moved.Keywords = []string{"$Notice", "Primary"} // old label still present via its mapped keyword
	srv.learnFromSyncedKeywords(u.ID, "Archive", []mailcache.Entry{moved})
	if n := len(corrections(t, store)); n != 0 {
		t.Fatal("only the INBOX window is the one the sorter labels")
	}
	srv.learnFromSyncedKeywords(u.ID, inbox, []mailcache.Entry{moved})
	if ex := corrections(t, store); len(ex) != 1 || ex[0].Label != "Primary" {
		t.Fatalf("corrections = %+v, want one Primary", ex)
	}

	reused := moved
	reused.Subject = "a different message that got UID 7"
	reused.Keywords = []string{"Promotions"}
	srv.learnFromSyncedKeywords(u.ID, inbox, []mailcache.Entry{reused})
	if ex := corrections(t, store); len(ex) != 1 || ex[0].Label != "Primary" {
		t.Fatalf("a reused UID changed the corrections: %+v", ex)
	}

	back := e // only Updates again
	srv.learnFromSyncedKeywords(u.ID, inbox, []mailcache.Entry{back})
	if n := len(corrections(t, store)); n != 0 {
		t.Fatalf("returning to the sorter's label left %d corrections", n)
	}
}

func TestLabelDescriptionsAreValidated(t *testing.T) {
	srv, u := newTestServerWithUser(t)
	put := func(body string) *httptest.ResponseRecorder {
		req := authedRequestForTest(httptest.NewRequest(http.MethodPut, "/api/labels/preferences", strings.NewReader(body)), u)
		rec := httptest.NewRecorder()
		srv.handleLabelPreferences(rec, req)
		return rec
	}
	for name, body := range map[string]string{
		"unknown label": `{"allowlist":["Primary"],"descriptions":{"Receipts":"order receipts"}}`,
		"too long":      `{"allowlist":["Primary"],"descriptions":{"Primary":"` + strings.Repeat("x", config.MaxLabelDescriptionRunes+1) + `"}}`,
		"control chars": `{"allowlist":["Primary"],"descriptions":{"Primary":"a\u0007b"}}`,
	} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if rec := put(`{"allowlist":["Primary","Receipts"],"descriptions":{"Receipts":"  Order receipts and invoices  ","Primary":"  "}}`); rec.Code != http.StatusOK {
		t.Fatalf("valid descriptions: status %d %s", rec.Code, rec.Body.String())
	}
	got := srv.userLabels(u.ID).Descriptions
	if len(got) != 1 || got["Receipts"] != "Order receipts and invoices" {
		t.Fatalf("stored descriptions = %v, want trimmed Receipts only", got)
	}
}
