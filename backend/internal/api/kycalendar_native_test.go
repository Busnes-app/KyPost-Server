//go:build linux

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

const nativeInviteICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:n1@example.test\r\nSUMMARY:Native planning\r\nORGANIZER:mailto:one@example.test\r\nATTENDEE:mailto:visible@outside.test\r\nATTENDEE:mailto:other@outside.test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// qualifyNativeITIP sends a KyCalendar invitation from a native mailbox
// through the real outbox and loopback TLS relay (TestNativeOutboundAPIActualTLSAndPGP
// modes kycalendar*). Refusals come first and dial nothing.
func qualifyNativeITIP(t *testing.T, s *Server, userID, mode string) {
	t.Helper()
	base, _ := url.Parse("https://cal.example.com")
	s.SetKyCalendar(kycalendar.New(base, kycalSecret, http.DefaultTransport))
	body := func(from string) map[string]any {
		return map[string]any{"sub": "native-runtime-one", "issuer": "https://idp.example", "from": from,
			"recipients": []string{"visible@outside.test", "other@outside.test"}, "method": "REQUEST", "ics": nativeInviteICS}
	}
	send := func(id, from string) (int, string) {
		rec := signedSend(t, s, kycalSecret, kycalendar.EventSend, id, body(from))
		return rec.Code, answerCode(rec)
	}
	if code, got := send("ev-off", "one@example.test"); code != 409 || got != "sending_off" {
		t.Fatal("switch off:", code, got)
	}
	on := true
	if err := config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error { st.KyCalendar = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"nobody@example.test", "one@outside.test"} {
		if code, got := send("ev-from-"+from, from); code != 409 || got != "bad_from" {
			t.Fatal("foreign from:", from, code, got)
		}
	}
	code, got := send("ev-1", "ONE@example.test")
	switch mode {
	case "kycalendar":
		if code != 202 || got != "sent" {
			t.Fatal("native invitation:", code, got)
		}
	default:
		if code != 503 || got != "smtp_failed" {
			t.Fatal("native failure:", code, got)
		}
	}
	// A refusal for good forgets the ID, so the retry is processed again
	// (here refused before sending); anything that went or may go out is kept.
	code, got = send("ev-1", "nobody@example.test")
	if want := map[string]string{"kycalendar": "duplicate", "kycalendar-lost-ack": "duplicate", "kycalendar-refused": "bad_from"}[mode]; got != want {
		t.Fatal("retry of ev-1:", code, got, "want", want)
	}
	a, _, err := s.nativeMailAssignment(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(userID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	messages, err := box.List(context.Background(), "Sent", 0, 10)
	if mode != "kycalendar" {
		if err != nil || len(messages) != 0 {
			t.Fatal("unsent invitation filed in Sent", messages, err)
		}
		return
	}
	if err != nil || len(messages) != 1 {
		t.Fatal("Sent missing or duplicated", messages, err)
	}
	sent, err := box.Raw(context.Background(), "Sent", messages[0].ID)
	if err != nil || !bytes.Contains(sent, []byte("text/calendar; method=REQUEST")) || !strings.Contains(string(sent), "Subject: Native planning") {
		t.Fatalf("Sent copy lacks the invitation: %v\n%s", err, sent)
	}
}
