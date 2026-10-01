package processor

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
	"github.com/Busnes-app/kypost-server/backend/internal/sorter"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// newHybridPoller is newTickTestPoller with auto-labelling on, the real
// embedding model, and a fake Ollama whose calls are counted. Skips without
// EMBED_MODEL_DIR (set in CI and in the image).
func newHybridPoller(t *testing.T, mail imapadapter.Client, allowlist []string, minConf float64) (*Poller, users.User, func() [][]string) {
	t.Helper()
	dir := os.Getenv("EMBED_MODEL_DIR")
	if dir == "" {
		t.Skip("EMBED_MODEL_DIR not set; the hybrid engine needs the real potion-base-8M files")
	}
	m, err := sorter.Load(dir)
	if err != nil {
		t.Fatalf("sorter.Load: %v", err)
	}
	p, u := newTickTestPoller(t, mail)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = true
	settings.Labels.Allowlist = allowlist
	settings.Labels.Seeded = true
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	if p.redaction, err = redaction.New(config.Default().Redaction.Patterns); err != nil {
		t.Fatal(err)
	}
	if p.globalStore, err = state.New(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var calls func() [][]string
	p.classifier, calls = fakeOllama(t, "Primary")
	p.SetSorter(m, minConf)
	return p, u, calls
}

var stockLabels = []string{"Primary", "Promotions", "Social", "Updates"}

// Mail the sorter is sure about is labelled without an LLM call, and so does
// not spend the LLM rate budget; the prediction is remembered for learning.
func TestHybrid_ConfidentMailSkipsTheLLMAndItsBudget(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "orders@shop.example", Subject: "Your order has shipped", Body: "Your package is on its way. Track it here."},
		{ID: "2", Sender: "deals@store.example", Subject: "40% off everything this weekend", Body: "Shop the sale now, limited time."},
	}}
	p, u, calls := newHybridPoller(t, mail, stockLabels, 0)
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 0, 0 // no LLM budget at all

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if want := []string{"1:Updates", "2:Promotions"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v", mail.labelled, want)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("LLM called %d times for mail the sorter was sure about", n)
	}
	store := tickStore(t, p, u)
	decisions, err := store.DecisionsStrict(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if !strings.Contains(d.Detail, "embedding sorter") {
			t.Fatalf("decision %s detail %q does not say which engine labelled it", d.MessageID, d.Detail)
		}
	}
	check, predicted, found, err := store.SorterPrediction("1")
	if err != nil || !found || predicted != "Updates" || check != state.SorterCheck("orders@shop.example", "Your order has shipped") {
		t.Fatalf("prediction not remembered: %q %q %v %v", check, predicted, found, err)
	}
}

// Below the threshold the LLM decides, exactly as before, and the LLM budget
// still defers what it cannot afford.
func TestHybrid_UnsureMailGoesToTheLLMAndRespectsTheBudget(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "a@example.com", Subject: "hello", Body: "hi"},
		{ID: "2", Sender: "b@example.com", Subject: "hello again", Body: "hi"},
	}}
	p, u, calls := newHybridPoller(t, mail, stockLabels, 1.01) // never confident
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 1, 1

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if n := len(calls()); n != 1 {
		t.Fatalf("LLM called %d times, want 1 (budget of one)", n)
	}
	if want := []string{"1:Primary"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v", mail.labelled, want)
	}
	store := tickStore(t, p, u)
	if got := checkpointOf(t, store); got != "1" {
		t.Fatalf("checkpoint = %q, want it held below the rate-deferred message", got)
	}
	// The LLM's answer is remembered too: a user relabelling it is a lesson.
	if _, predicted, found, _ := store.SorterPrediction("1"); !found || predicted != "Primary" {
		t.Fatalf("LLM-labelled prediction not remembered: %q %v", predicted, found)
	}
}

// An account's label list is user-controlled and unbounded, and every label is a
// class the head must fit on the daemon every account shares. Over the label
// limit the account is sorted by the LLM alone — no training at all — and the
// next account's poll is unaffected.
func TestHybrid_TooManyLabelsSkipsTrainingAndSparesOtherAccounts(t *testing.T) {
	big := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "orders@shop.example", Subject: "Your order has shipped", Body: "Your package is on its way."},
	}}
	p, heavy, calls := newHybridPoller(t, big, stockLabels, 0)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled, settings.Labels.Seeded = true, true
	settings.Labels.Allowlist = []string{"Primary"}
	settings.Labels.Descriptions = map[string]string{}
	for i := range sorter.MaxLabels * 5 {
		label := fmt.Sprintf("Label%d", i)
		settings.Labels.Allowlist = append(settings.Labels.Allowlist, label)
		settings.Labels.Descriptions[label] = strings.Repeat("a long description of what goes here ", 5)
	}
	if err := config.SaveUserSettings(p.userSettingsPath(heavy.ID), settings); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := p.tickUser(heavy, time.Time{}); err != nil {
		t.Fatalf("tickUser (heavy account): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("over-limit account's tick took %v; training should have been skipped", elapsed)
	}
	p.userMu.Lock()
	cached := p.heads[heavy.ID]
	p.userMu.Unlock()
	if cached == nil || cached.head != nil {
		t.Fatalf("over-limit account: cached=%+v, want a cached LLM-only (nil) head", cached)
	}
	if want := []string{"1:Primary"}; !reflect.DeepEqual(big.labelled, want) || len(calls()) != 1 {
		t.Fatalf("over-limit account must still be sorted by the LLM: labelled=%v, LLM calls=%d", big.labelled, len(calls()))
	}

	// A second account, polled next, still gets the sorter.
	normal := users.User{ID: "user-normal", Username: "normal"}
	small := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "deals@store.example", Subject: "40% off everything this weekend", Body: "Shop the sale now, limited time."},
	}}
	p.newMailClient = func(string, string) imapadapter.Client { return small }
	if err := os.MkdirAll(p.userConfigDir(normal.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	ns := config.DefaultUserSettings()
	ns.Labels.AutoApplyEnabled, ns.Labels.Seeded, ns.Labels.Allowlist = true, true, stockLabels
	if err := config.SaveUserSettings(p.userSettingsPath(normal.ID), ns); err != nil {
		t.Fatal(err)
	}
	if err := p.tickUser(normal, time.Time{}); err != nil {
		t.Fatalf("tickUser (normal account): %v", err)
	}
	if want := []string{"1:Promotions"}; !reflect.DeepEqual(small.labelled, want) || len(calls()) != 1 {
		t.Fatalf("normal account: labelled=%v, LLM calls=%d; want the sorter to label it", small.labelled, len(calls()))
	}
}

// A label the shipped model has never heard of is learned from the user's
// corrections, and the next tick uses it without a restart.
func TestHybrid_CorrectionsTeachANewLabel(t *testing.T) {
	receipt := func(id, shop string) imapadapter.Message {
		return imapadapter.Message{ID: id, Sender: "billing@" + shop + ".example",
			Subject: "Your receipt from " + shop, Body: "Thanks for your purchase. Total charged to your card ending 4421. Receipt attached."}
	}
	mail := &scriptedMailbox{}
	for i, shop := range []string{"alpha", "bravo", "charlie", "delta"} {
		mail.msgs = append(mail.msgs, receipt(fmt.Sprint(i+1), shop))
	}
	labels := append(slices.Clone(stockLabels), "Receipts")
	p, u, _ := newHybridPoller(t, mail, labels, 0)
	store := tickStore(t, p, u)

	mail.msgs, mail.labelled = mail.msgs[:3], nil
	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, line := range mail.labelled {
		if strings.HasSuffix(line, ":Receipts") {
			t.Fatalf("predicted a label with no examples: %v", mail.labelled)
		}
	}
	// The user files the three receipts under Receipts (what the API records).
	for _, m := range mail.msgs {
		if learned, err := store.RecordSorterCorrection(m.ID, state.SorterCheck(m.Sender, m.Subject), "Receipts"); err != nil || !learned {
			t.Fatalf("correction %s: learned=%v err=%v", m.ID, learned, err)
		}
	}
	mail.msgs = append(mail.msgs, receipt("4", "delta"))
	mail.labelled = nil
	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"4:Receipts"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("after 3 corrections labelled = %v, want %v", mail.labelled, want)
	}
}
