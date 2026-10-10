package imap

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// An iMIP invite is usually a multipart/alternative body part with no
// Content-Disposition, which enmime files under OtherParts and nothing listed.
const inviteMessage = "From: organizer@x.test\r\nTo: b@x.test\r\nSubject: Invitation\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=M\r\n\r\n" +
	"--M\r\nContent-Type: multipart/alternative; boundary=A\r\n\r\n" +
	"--A\r\nContent-Type: text/plain\r\n\r\nYou are invited\r\n" +
	"--A\r\nContent-Type: text/calendar; method=REQUEST; charset=UTF-8\r\n\r\n" +
	"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:e1@x.test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n" +
	"--A--\r\n" +
	"--M\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=agenda.pdf\r\n\r\n%PDF-1\r\n" +
	"--M--\r\n"

func TestParseRawContentExposesUndisposedInvite(t *testing.T) {
	parsed, err := ParseRawContent([]byte(inviteMessage))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Attachments) != 2 || parsed.Attachments[0].Name != "agenda.pdf" {
		t.Fatalf("attachments = %+v; the real attachment must keep index 0", parsed.Attachments)
	}
	info := NewAttachmentInfo(1, parsed.Attachments[1])
	if info.Name != "invite.ics" || info.MimeType != "text/calendar" || info.CalendarMethod != "REQUEST" ||
		!strings.Contains(string(parsed.Attachments[1].Content), "UID:e1@x.test") {
		t.Fatalf("invite = %+v", info)
	}
	if !parsed.Content.HasAttachments || parsed.Content.Body != "You are invited" {
		t.Fatalf("content = %+v", parsed.Content)
	}
}

func TestCalendarMethod(t *testing.T) {
	for ics, want := range map[string]string{
		"BEGIN:VCALENDAR\r\nMETHOD:cancel\r\nBEGIN:VEVENT\r\n":          "CANCEL",
		"BEGIN:VCALENDAR\nMETHOD:REQUEST\n":                             "REQUEST",
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nMETHOD:REQUEST\r\n":         "",
		"BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nX: <script>\r\n":          "REQUEST",
		"BEGIN:VCALENDAR\r\nMETHOD:<script>alert(1)</script>\r\n":       "",
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\n":                            "",
		"not calendar":                                                  "",
		"BEGIN:VCALENDAR\r\nMETHOD:" + strings.Repeat("A", 40) + "\r\n": "",
	} {
		if got := CalendarMethod([]byte(ics)); got != want {
			t.Errorf("CalendarMethod(%q) = %q, want %q", ics, got, want)
		}
	}
}

// The IMAP backend lists the invite too: same parser as native.
func TestIMAPListAttachmentsIncludesInvite(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		switch {
		case strings.HasPrefix(command, "UID SEARCH"):
			return "* SEARCH\r\n" + tag + " OK done\r\n", true
		case strings.HasPrefix(command, "UID FETCH 7 BODY.PEEK[]"):
			return fmt.Sprintf("* 1 FETCH (UID 7 BODY[] {%d}\r\n%s)\r\n", len(inviteMessage), inviteMessage) + tag + " OK done\r\n", true
		}
		return "", false
	}
	server.mu.Unlock()
	client := server.client("INBOX")

	infos, err := client.ListAttachments(context.Background(), "INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[1].Name != "invite.ics" || infos[1].CalendarMethod != "REQUEST" {
		t.Fatalf("infos = %+v", infos)
	}
	info, content, err := client.GetAttachment(context.Background(), "INBOX", 7, 1)
	if err != nil || info.MimeType != "text/calendar" || !strings.HasPrefix(string(content), "BEGIN:VCALENDAR") {
		t.Fatalf("get = %+v %q %v", info, content, err)
	}
}
