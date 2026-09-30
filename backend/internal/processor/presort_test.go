package processor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestIsBulkOrAutomated(t *testing.T) {
	for _, tc := range []struct {
		h    map[string][]string
		want bool
	}{
		{map[string][]string{}, false},
		{map[string][]string{"List-Id": {"<news.example.com>"}}, true},
		{map[string][]string{"List-Unsubscribe": {"<mailto:u@example.com>"}}, true},
		{map[string][]string{"Precedence": {" Bulk "}}, true},
		{map[string][]string{"Precedence": {"junk"}}, true},
		{map[string][]string{"Precedence": {"first-class"}}, false},
		{map[string][]string{"Auto-Submitted": {"auto-generated"}}, true},
		{map[string][]string{"Auto-Submitted": {"no"}}, false},
	} {
		if got := isBulkOrAutomated(tc.h); got != tc.want {
			t.Errorf("isBulkOrAutomated(%v) = %v, want %v", tc.h, got, tc.want)
		}
	}
}

func TestHeadersByUIDKeepsEveryCopyAndMarksFetched(t *testing.T) {
	// UID 2 was fetched with none of the headers; UID 3 was not fetched at all.
	got := headersByUID(map[int][]string{1: {"x-spam-flag: NO", "X-Spam-Flag: YES", "List-Id: <l>"}, 2: {}})
	want := map[int]map[string][]string{
		1: {"X-Spam-Flag": {"NO", "YES"}, "List-Id": {"<l>"}},
		2: {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("headersByUID = %v, want %v", got, want)
	}
}

func TestKnownSendersExcludesDiscoveryCreatedContacts(t *testing.T) {
	store, err := contacts.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(contacts.Contact{FormattedName: "Dana", Emails: []contacts.ContactValue{{Value: " Dana@Example.com "}}}); err != nil {
		t.Fatal(err)
	}
	// Autocrypt harvest creates exactly this from a stranger's first message.
	if _, err := store.Upsert(contacts.Contact{FormattedName: "x@evil.example", Emails: []contacts.ContactValue{{Value: "x@evil.example"}}, DiscoveryCreated: true}); err != nil {
		t.Fatal(err)
	}
	got, err := knownSenders(store)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"dana@example.com": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("knownSenders = %v, want %v", got, want)
	}
}

func TestPresortDecisions(t *testing.T) {
	stock := []string{"Primary", "Promotions", "Social", "Updates"}
	bulk := map[string][]string{"List-Unsubscribe": {"<https://x>"}}
	known := map[string]bool{"dana@example.com": true}
	contact := imapadapter.Message{Sender: "Dana <dana@example.com>"}
	stranger := imapadapter.Message{Sender: "shop@example.com"}
	for _, tc := range []struct {
		name      string
		allowlist []string
		headers   map[string][]string
		msg       imapadapter.Message
		label     string
		allowed   []string
	}{
		{"contact skips the LLM", stock, map[string][]string{}, contact, "Primary", nil},
		{"contact's list mail still goes to the LLM", stock, bulk, contact, "", []string{"Promotions", "Social", "Updates"}},
		{"bulk stranger loses Primary", stock, bulk, stranger, "", []string{"Promotions", "Social", "Updates"}},
		{"plain stranger unchanged", stock, map[string][]string{}, stranger, "", stock},
		{"custom label set untouched", []string{"Work", "Home"}, bulk, contact, "", []string{"Work", "Home"}},
		{"Primary-only set not emptied", []string{"Primary"}, bulk, stranger, "", []string{"Primary"}},
		{"oversized contact mail takes no shortcut", stock, nil, imapadapter.Message{Sender: "dana@example.com", TooLarge: true}, "", stock},
	} {
		uc := userCtx{allowlist: tc.allowlist, headers: map[int]map[string][]string{7: tc.headers}, knownSenders: known}
		label, allowed, _ := presort(uc, tc.msg, 7)
		if label != tc.label || !reflect.DeepEqual(allowed, tc.allowed) {
			t.Errorf("%s: presort = (%q, %v), want (%q, %v)", tc.name, label, allowed, tc.label, tc.allowed)
		}
	}
	// Contacts unreadable this tick: no known-sender shortcut.
	uc := userCtx{allowlist: stock, headers: map[int]map[string][]string{7: {}}}
	if label, _, _ := presort(uc, contact, 7); label != "" {
		t.Errorf("presort with nil knownSenders = %q, want no label", label)
	}
}

// headerMailbox serves FetchHeaderFields from a per-UID script. Its first call
// is the tick's batch fetch; later ones are Autocrypt harvest.
type headerMailbox struct {
	scriptedMailbox
	headerLines map[int][]string
	headerErr   error
	askedFor    []string
	batchUIDs   []int
	calls       int
}

func (m *headerMailbox) FetchHeaderFields(_ context.Context, _ string, uids []int, fields ...string) (map[int][]string, error) {
	if m.calls++; m.calls == 1 {
		m.batchUIDs = uids
	}
	m.askedFor = append(m.askedFor, fields...)
	return m.headerLines, m.headerErr
}

// A UID the adapter skipped (past its byte budget) has unknown headers; with a
// header rule configured it waits for a later tick instead of being retired
// without the rule.
func TestTickUser_UnfetchedHeadersHoldMessageForHeaderRules(t *testing.T) {
	mail := &headerMailbox{
		scriptedMailbox: scriptedMailbox{msgs: []imapadapter.Message{
			{ID: "1", Sender: "a@example.com", Subject: "s", Body: "b"},
			{ID: "2", Sender: "b@example.com", Subject: "s", Body: "b"},
		}},
		headerLines: map[int][]string{2: {}},
	}
	p, u := newTickTestPoller(t, mail)
	rs, err := p.userRulesStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Upsert(rules.Rule{Name: "spam", Enabled: true,
		Match:   rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "header", Header: "X-Spam-Flag", Comparator: "is", Value: "YES"}}},
		Actions: []rules.Action{{Type: "spam"}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if want := []string{"2:Primary"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v: UID 1 must wait for its headers", mail.labelled, want)
	}
	store, err := p.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cp, _ := store.Checkpoint(); cp != "" && cp != "0" {
		t.Fatalf("checkpoint advanced to %q past the held UID 1", cp)
	}
}

func TestTickUser_HeaderFetchFailure(t *testing.T) {
	newMail := func() *headerMailbox {
		return &headerMailbox{
			scriptedMailbox: scriptedMailbox{msgs: []imapadapter.Message{
				{ID: "1", Sender: "a@example.com", Subject: "s", Body: "b"},
				{ID: "2", Sender: "b@example.com", TooLarge: true},
			}},
			headerErr: errors.New("connection reset"),
		}
	}

	t.Run("pre-sort only: carries on without the bulk check", func(t *testing.T) {
		mail := newMail()
		p, u := newTickTestPoller(t, mail)
		if err := p.tickUser(u, time.Time{}); err != nil {
			t.Fatalf("tickUser: %v", err)
		}
		if !reflect.DeepEqual(mail.batchUIDs, []int{1}) {
			t.Fatalf("batch header fetch asked for UIDs %v, want [1]: TooLarge mail must not be fetched", mail.batchUIDs)
		}
		if len(mail.labelled) == 0 {
			t.Fatal("no message was labelled; a pre-sort header failure must not block mail")
		}
	})

	t.Run("header rule present: holds messages and fails the tick", func(t *testing.T) {
		mail := newMail()
		p, u := newTickTestPoller(t, mail)
		rs, err := p.userRulesStore(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rs.Upsert(rules.Rule{Name: "spam", Enabled: true,
			Match:   rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "header", Header: "X-Spam-Flag", Comparator: "is", Value: "YES"}}},
			Actions: []rules.Action{{Type: "spam"}}}); err != nil {
			t.Fatal(err)
		}
		if err := p.tickUser(u, time.Time{}); err == nil {
			t.Fatal("tickUser returned nil; a header rule that cannot see headers must fail the tick")
		}
		if len(mail.labelled) != 0 || len(mail.inboxActions) != 0 {
			t.Fatalf("labelled %v / actions %v; messages must be held, not processed blind", mail.labelled, mail.inboxActions)
		}
		store, err := p.userStore(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cp, _ := store.Checkpoint(); cp != "" && cp != "0" {
			t.Fatalf("checkpoint advanced to %q past held messages", cp)
		}
	})
}

// fakeOllama answers warmup and records the label enum of every classify call.
func fakeOllama(t *testing.T, answer string) (*classifier.HTTPClient, func() [][]string) {
	var mu sync.Mutex
	var enums [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			var body struct {
				Format struct {
					Enum []string `json:"enum"`
				} `json:"format"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Format.Enum != nil {
				mu.Lock()
				enums = append(enums, body.Format.Enum)
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": `"` + answer + `"`, "done": true})
			return
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(srv.Close)
	c := classifier.NewHTTPClient(srv.URL, "", "/api/generate", "", 5*time.Second)
	return c, func() [][]string { mu.Lock(); defer mu.Unlock(); return enums }
}

func TestTickUser_PresortsContactsAndNarrowsBulkMail(t *testing.T) {
	mail := &headerMailbox{
		scriptedMailbox: scriptedMailbox{msgs: []imapadapter.Message{
			{ID: "1", Sender: "Dana <dana@example.com>", Subject: "lunch?", Body: "Are you free Thursday?"},
			{ID: "2", Sender: "news@shop.example", Subject: "Sale", Body: "40% off"},
		}},
		headerLines: map[int][]string{2: {"List-Unsubscribe: <https://shop.example/u>"}},
	}
	p, u := newTickTestPoller(t, mail)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = true
	settings.Labels.Allowlist = []string{"Primary", "Promotions", "Updates"}
	settings.Labels.Seeded = true
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	cs, err := p.userContactsStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Upsert(contacts.Contact{FormattedName: "Dana", Emails: []contacts.ContactValue{{Value: "dana@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if p.redaction, err = redaction.New(config.Default().Redaction.Patterns); err != nil {
		t.Fatal(err)
	}
	if p.globalStore, err = state.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var enums func() [][]string
	p.classifier, enums = fakeOllama(t, "Promotions")
	// Exactly one classifier call's worth of budget: the contact must not use it.
	p.cfg.RateLimits.PerMinute = 1
	p.cfg.RateLimits.PerHour = 1

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if want := []string{"1:Primary", "2:Promotions"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v", mail.labelled, want)
	}
	if got := enums(); len(got) != 1 || !reflect.DeepEqual(got[0], []string{"Promotions", "Updates"}) {
		t.Fatalf("classifier label enums = %v, want one call offering [Promotions Updates]", got)
	}
	if !strings.Contains(strings.Join(mail.askedFor, " "), "List-Unsubscribe") {
		t.Fatalf("header fetch asked for %v, want the pre-sort headers", mail.askedFor)
	}
	store, err := p.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := store.DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	details := map[string]string{}
	for _, d := range decisions {
		details[d.MessageID] = d.Detail
	}
	if !strings.Contains(details["1"], "sender is a contact") || !strings.Contains(details["2"], "Primary excluded") {
		t.Fatalf("decision details = %v, want the pre-sort reasons recorded", details)
	}
}
