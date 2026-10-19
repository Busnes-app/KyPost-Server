package mailmsg

import (
	"bytes"
	"errors"
	"mime"
	"strings"
	"testing"
	"time"

	"github.com/jhillyerd/enmime/v2"
)

const validReply = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//KyPost//EN\nMETHOD:REPLY\nBEGIN:VEVENT\nUID:e1@x.test\nATTENDEE;PARTSTAT=ACCEPTED:mailto:b@x.test\n DESCRIPTION folded\nEND:VEVENT\nEND:VCALENDAR\n"

func TestCalendarReplyNormalisesAValidReply(t *testing.T) {
	got, err := CalendarReply(validReply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ReplaceAll(string(got), "\r\n", ""), "\n") || !strings.HasSuffix(string(got), "END:VCALENDAR\r\n") {
		t.Fatalf("not CRLF-normalised: %q", got)
	}
}

// RFC 5545 §3.1 folding: a property may be split anywhere, including inside
// METHOD's value or a BEGIN/END line, and must be unfolded before checking.
func TestCalendarReplyUnfoldsFoldedLines(t *testing.T) {
	for name, ics := range map[string]string{
		"folded METHOD": strings.Replace(validReply, "METHOD:REPLY", "METHOD:REPL\n Y", 1),
		"folded BEGIN":  strings.Replace(validReply, "BEGIN:VEVENT", "BEGIN:VEV\n\tENT", 1),
		"folded END":    strings.Replace(validReply, "END:VEVENT", "END:VE\r\n VENT", 1),
	} {
		got, err := CalendarReply(ics)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(string(got), "\r\n ") && !strings.Contains(string(got), "\r\n\t") {
			t.Errorf("%s: folding not preserved in the sent bytes: %q", name, got)
		}
	}
	// A fold that hides a second VEVENT is still counted.
	hidden := strings.Replace(validReply, "END:VEVENT", "END:VEVENT\nBEGIN:VEV\n ENT\nUID:x\nEND:VEVENT", 1)
	if _, err := CalendarReply(hidden); !errors.Is(err, ErrCalendarReply) {
		t.Fatalf("folded second VEVENT accepted: %v", err)
	}
}

func TestCalendarReplyRefusals(t *testing.T) {
	swap := func(old, new string) string { return strings.Replace(validReply, old, new, 1) }
	for name, ics := range map[string]string{
		"too large":         validReply + strings.Repeat("X", MaxCalendarReplyBytes),
		"not UTF-8":         swap("PRODID:-//KyPost//EN", "PRODID:\xff"),
		"control character": swap("PRODID:-//KyPost//EN", "PRODID:a\x00b"),
		"header injection":  swap("PRODID:-//KyPost//EN", "PRODID:x\r\n\r\nBcc: victim@x.test"),
		"request not reply": swap("METHOD:REPLY", "METHOD:REQUEST"),
		"no method":         swap("METHOD:REPLY\n", ""),
		"two methods":       swap("METHOD:REPLY", "METHOD:REPLY\nMETHOD:REPLY"),
		"method in event":   swap("METHOD:REPLY\nBEGIN:VEVENT", "BEGIN:VEVENT\nMETHOD:REPLY"),
		"no event":          swap("BEGIN:VEVENT\nUID:e1@x.test\nATTENDEE;PARTSTAT=ACCEPTED:mailto:b@x.test\n DESCRIPTION folded\nEND:VEVENT\n", ""),
		"two events":        swap("END:VEVENT", "END:VEVENT\nBEGIN:VEVENT\nUID:e2\nEND:VEVENT"),
		"unbalanced":        swap("END:VEVENT\n", ""),
		"trailing content":  validReply + "BEGIN:VCALENDAR\n",
		"not a calendar":    "hello",
		"leading fold":      " " + validReply,
		"too deep":          swap("BEGIN:VEVENT\n", "BEGIN:VEVENT\n"+strings.Repeat("BEGIN:X\n", 8)+strings.Repeat("END:X\n", 8)),
	} {
		if _, err := CalendarReply(ics); !errors.Is(err, ErrCalendarReply) {
			t.Errorf("%s: err = %v, want ErrCalendarReply", name, err)
		}
	}
}

// The reply must be a text/calendar; method=REPLY part alternative to the
// text body, inside multipart/mixed when there are attachments.
func TestBuildCalendarReplyMIME(t *testing.T) {
	ics, err := CalendarReply(validReply)
	if err != nil {
		t.Fatal(err)
	}
	for _, withAttachment := range []bool{false, true} {
		m := Message{From: "b@x.test", To: []string{"organizer@x.test"}, Subject: "Accepted: e1", Body: "Accepted", Calendar: ics}
		if withAttachment {
			m.Attachments = []Attachment{{Name: "note.txt", MimeType: "text/plain", Content: []byte("n")}}
		}
		raw := m.Build()
		env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var cal *enmime.Part
		var walk func(p *enmime.Part)
		walk = func(p *enmime.Part) {
			for ; p != nil; p = p.NextSibling {
				if p.ContentType == "text/calendar" {
					cal = p
				}
				walk(p.FirstChild)
			}
		}
		walk(env.Root)
		if cal == nil || cal.Parent == nil || cal.Parent.ContentType != "multipart/alternative" || !bytes.Equal(cal.Content, ics) {
			t.Fatalf("attachment=%v: calendar part missing or misplaced:\n%s", withAttachment, raw)
		}
		_, params, _ := mime.ParseMediaType(cal.Header.Get("Content-Type"))
		if params["method"] != "REPLY" || params["charset"] != "UTF-8" {
			t.Fatalf("calendar params = %v", params)
		}
		if env.Text != "Accepted" || (withAttachment && len(env.Attachments) != 1) {
			t.Fatalf("attachment=%v: body %q attachments %d", withAttachment, env.Text, len(env.Attachments))
		}
	}
}

const validRequest = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:e1@x.test\r\n" +
	"SUMMARY:Plan\\, review\r\nORGANIZER;CN=\"Org: Lead\":mailto:Org@X.test\r\nATTENDEE;CN=A:mailto:a@x.test\r\n" +
	"ATTENDEE:mailto:b@x.test\r\nBEGIN:VALARM\r\nACTION:EMAIL\r\nATTENDEE:mailto:alarm@x.test\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:e1@x.test\r\nRECURRENCE-ID:20261020T100000Z\r\nORGANIZER:mailto:org@x.test\r\nATTENDEE:mailto:c@x.test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestParseITIPReadsTheObject(t *testing.T) {
	it, raw, err := ParseITIP(validRequest, "REQUEST")
	if err != nil {
		t.Fatal(err)
	}
	if it.UID != "e1@x.test" || it.Organizer != "org@x.test" || it.Summary != "Plan, review" ||
		strings.Join(it.Attendees, ",") != "a@x.test,b@x.test,c@x.test" || string(raw) != validRequest {
		t.Fatalf("ParseITIP = %+v", it)
	}
	if got := ITIPSubject(it); got != "Plan, review" {
		t.Fatalf("subject %q", got)
	}
}

func TestParseITIPRefusals(t *testing.T) {
	swap := func(old, new string) string { return strings.Replace(validRequest, old, new, 1) }
	for name, tc := range map[string]struct{ ics, method string }{
		"method mismatch":   {validRequest, "CANCEL"},
		"no organizer":      {swap("ORGANIZER;CN=\"Org: Lead\":mailto:Org@X.test\r\n", ""), "REQUEST"},
		"no uid":            {swap("UID:e1@x.test\r\nSUMMARY", "SUMMARY"), "REQUEST"},
		"two uids":          {swap("UID:e1@x.test\r\nRECURRENCE", "UID:e2@x.test\r\nRECURRENCE"), "REQUEST"},
		"two organizers":    {swap("ORGANIZER:mailto:org@x.test", "ORGANIZER:mailto:other@x.test"), "REQUEST"},
		"non-mailto org":    {strings.ReplaceAll(swap("ORGANIZER;CN=\"Org: Lead\":mailto:Org@X.test", "ORGANIZER:urn:x"), "ORGANIZER:mailto:org@x.test", "ORGANIZER:urn:x"), "REQUEST"},
		"no event":          {"BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n", "REQUEST"},
		"too large":         {validRequest + strings.Repeat("X", MaxITIPBytes), "REQUEST"},
		"long parameter":    {swap("CN=A", "CN="+strings.Repeat("a", 1025)), "REQUEST"},
		"long quoted param": {swap("CN=A", "CN=\""+strings.Repeat("a:", 513)+"\""), "REQUEST"},
		"header injection":  {swap("SUMMARY:Plan\\, review", "SUMMARY:x\r\n\r\nBcc: v@x.test"), "REQUEST"},
		"two calendars":     {validRequest + validRequest, "REQUEST"},
	} {
		if _, _, err := ParseITIP(tc.ics, tc.method); !errors.Is(err, ErrITIP) {
			t.Errorf("%s: err = %v, want ErrITIP", name, err)
		}
	}
	// A 1 KiB parameter is fine; only longer ones are refused.
	if _, _, err := ParseITIP(swap("CN=A", "CN="+strings.Repeat("a", 1024)), "REQUEST"); err != nil {
		t.Fatalf("1 KiB parameter refused: %v", err)
	}
}

// Folding a megabyte into one property stays linear: the scan unfolds with a
// builder, not by repeated concatenation.
func TestParseITIPLongFoldIsLinear(t *testing.T) {
	ics := strings.Replace(validRequest, "SUMMARY:Plan\\, review", "SUMMARY:x"+strings.Repeat("\r\n y", 250000), 1)
	start := time.Now()
	if _, _, err := ParseITIP(ics, "REQUEST"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("parse took %v", d)
	}
}

func TestITIPSubject(t *testing.T) {
	long := strings.Repeat("é", 150)
	for _, tc := range []struct {
		it   ITIP
		want string
	}{
		{ITIP{Method: "REQUEST"}, "Invitation"},
		{ITIP{Method: "CANCEL"}, "Cancelled"},
		{ITIP{Method: "REPLY", Summary: " \t "}, "Reply"},
		{ITIP{Method: "REQUEST", Summary: "a\tb\x7fc"}, "a b c"},
		{ITIP{Method: "REQUEST", Summary: long}, strings.Repeat("é", 100)},
	} {
		if got := ITIPSubject(tc.it); got != tc.want {
			t.Errorf("ITIPSubject(%q) = %q", tc.it.Summary, got)
		}
	}
}

func TestBuildCalendarMethod(t *testing.T) {
	for method, want := range map[string]string{"REQUEST": "REQUEST", "CANCEL": "CANCEL", "": "REPLY", "X\r\nBcc: v": "REPLY"} {
		raw := string(Message{From: "a@x.test", To: []string{"b@x.test"}, Body: "b", Calendar: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), CalendarMethod: method}.Build())
		if !strings.Contains(raw, "Content-Type: text/calendar; method="+want+"; charset=UTF-8\r\n") || strings.Contains(raw, "Bcc") {
			t.Errorf("method %q:\n%s", method, raw)
		}
	}
}
