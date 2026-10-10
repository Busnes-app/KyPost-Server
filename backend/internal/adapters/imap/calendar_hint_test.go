package imap

import (
	"context"
	"strconv"
	"strings"
	"testing"

	goimap "github.com/BrianLeishman/go-imap"
)

// An Outlook/Google invite is a text/calendar alternative with no disposition,
// which go-imap's parser drops, so the list said hasAttachments:false and the
// app never asked for the invite. The hint reads BODYSTRUCTURE for messages
// that have no attachment, and only part positions count — a parameter list
// spelling ("text" "calendar") must not match.
func TestCalendarInviteUIDsReadsBodyStructure(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	var asked string
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		if !strings.HasSuffix(command, "BODYSTRUCTURE") {
			return "", false
		}
		asked = command
		return `* 1 FETCH (UID 7 BODYSTRUCTURE (("TEXT" "PLAIN" ("CHARSET" "utf-8") NIL NIL "7BIT" 5 1 NIL NIL NIL NIL)("TEXT" "CALENDAR" ("METHOD" "REQUEST" "CHARSET" "UTF-8") NIL NIL "7BIT" 90 4 NIL NIL NIL NIL) "ALTERNATIVE" ("BOUNDARY" "A") NIL NIL NIL))` + "\r\n" +
			`* 2 FETCH (UID 8 BODYSTRUCTURE ("TEXT" "PLAIN" ("TEXT" "CALENDAR") NIL NIL "7BIT" 5 1 NIL NIL NIL NIL))` + "\r\n" +
			`* 3 FETCH (UID 9 BODYSTRUCTURE ((("TEXT" "PLAIN" NIL NIL NIL "7BIT" 5 1 NIL NIL NIL NIL)("text" "calendar" NIL NIL NIL "BASE64" 90 2 NIL NIL NIL NIL) "ALTERNATIVE" NIL NIL NIL NIL) "MIXED" NIL NIL NIL NIL))` + "\r\n" +
			tag + " OK done\r\n", true
	}
	server.mu.Unlock()
	client := server.client("INBOX")
	client.opMu.Lock()
	d, err := client.ensureConnectedLocked()
	client.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	emails := map[int]*goimap.Email{
		6: {Attachments: []goimap.Attachment{{Name: "a.pdf"}}},
		7: {}, 8: {}, 9: {},
	}
	got := calendarInviteUIDs(d, emails)
	if !got[7] || got[8] || !got[9] || len(got) != 2 {
		t.Fatalf("invites = %v, want 7 and 9", got)
	}
	if asked != "UID FETCH 7,8,9 BODYSTRUCTURE" {
		t.Fatalf("asked %q; a message with attachments must not be fetched again", asked)
	}
}

// The list path wires the hint into HasAttachments.
func TestGetMessageBodiesFlagsInviteOnlyMessage(t *testing.T) {
	quietRetries(t, 0)
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	const raw = "From: o@x.test\r\nSubject: Invitation\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=A\r\n\r\n" +
		"--A\r\nContent-Type: text/plain\r\n\r\ninvited\r\n--A\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n--A--\r\n"
	server.mu.Lock()
	server.commandHook = func(tag, command string) (string, bool) {
		ok := tag + " OK done\r\n"
		switch {
		case strings.HasPrefix(command, "UID SEARCH"):
			return "* SEARCH\r\n" + ok, true
		case command == "UID FETCH 7 ALL":
			return `* 1 FETCH (UID 7 FLAGS () INTERNALDATE "02-Oct-2026 12:00:00 +0000" RFC822.SIZE 300 ENVELOPE ("Fri, 2 Oct 2026 12:00:00 +0000" "Invitation" (("O" NIL "o" "x.test")) NIL NIL NIL NIL NIL NIL "<i@x.test>"))` + "\r\n" + ok, true
		case command == "UID FETCH 7 BODY.PEEK[]":
			return "* 1 FETCH (UID 7 BODY[] {" + strconv.Itoa(len(raw)) + "}\r\n" + raw + ")\r\n" + ok, true
		case command == "UID FETCH 7 BODYSTRUCTURE":
			return `* 1 FETCH (UID 7 BODYSTRUCTURE (("TEXT" "PLAIN" NIL NIL NIL "7BIT" 7 1 NIL NIL NIL NIL)("TEXT" "CALENDAR" ("METHOD" "REQUEST") NIL NIL "7BIT" 40 3 NIL NIL NIL NIL) "ALTERNATIVE" ("BOUNDARY" "A") NIL NIL NIL))` + "\r\n" + ok, true
		}
		return "", false
	}
	server.mu.Unlock()
	got, err := server.client("INBOX").GetMessageBodies(context.Background(), "INBOX", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[7]; c.Body != "invited" || !c.HasAttachments {
		t.Fatalf("content = %+v, want body and HasAttachments", c)
	}
}
