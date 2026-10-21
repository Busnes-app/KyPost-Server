package processor

import (
	"reflect"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
)

// KYPOST_DISABLE_AI_SORTING overrides a user who left auto-apply on: their
// mail gets the fallback label and Ollama is never called.
func TestDisableAISorting_OverridesAutoApply(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "deals@store.example", Subject: "40% off everything", Body: "Shop the sale now."},
	}}
	p, u := newTickTestPoller(t, mail)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = true
	settings.Labels.Allowlist = []string{"Promotions", "Primary"}
	settings.Labels.Seeded = true
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	var calls func() [][]string
	p.classifier, calls = fakeOllama(t, "Promotions")
	p.DisableAISorting()

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if want := []string{"1:Primary"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v", mail.labelled, want)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("Ollama called %d times with AI sorting disabled", n)
	}
}

// Mail that already carries a label keyword keeps that label when the
// classifier is skipped; the fallback must not be stacked on as a second tab.
func TestAutoLabelOff_KeepsExistingLabel(t *testing.T) {
	mail := &scriptedMailbox{msgs: []imapadapter.Message{
		{ID: "1", Sender: "deals@store.example", Subject: "40% off", Body: "Sale.", Keywords: []string{"promotions"}},
		{ID: "2", Sender: "friend@example.com", Subject: "Hi", Body: "Lunch?"},
	}}
	p, u := newTickTestPoller(t, mail)
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = false
	settings.Labels.Allowlist = []string{"Primary", "Promotions"}
	settings.Labels.Seeded = true
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}

	if err := p.tickUser(u, time.Time{}); err != nil {
		t.Fatalf("tickUser: %v", err)
	}
	if want := []string{"1:Promotions", "2:Primary"}; !reflect.DeepEqual(mail.labelled, want) {
		t.Fatalf("labelled = %v, want %v", mail.labelled, want)
	}
}
