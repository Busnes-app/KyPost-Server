package mailmsg

import (
	"bytes"
	"net/mail"
	"regexp"
	"strings"
	"testing"
)

var wholeMsgID = regexp.MustCompile(`^` + msgIDPattern.String() + `$`)

// FuzzReplyThreading feeds a received message's Message-ID, References and
// In-Reply-To (all sender-controlled) through ReplyThreading into a reply. Only
// well-formed msg-ids may come out, and the reply's header block must parse to
// exactly the headers Build writes: nothing injected, no line over 998 octets.
func FuzzReplyThreading(f *testing.F) {
	f.Add("<b@x.test>", "<root@x.test> <a@x.test>", "")
	f.Add("<b@x.test>", "", "<a@x.test>")
	f.Add("<a@x.test> <b@x.test>", "<r@x.test>", "")
	f.Add("<r29@x.test>", "<a@x.test>\r\nBcc: victim@x.test\r\n (comment) <b@x.test>junk<c d@x.test>", "")

	want := map[string]bool{"From": true, "To": true, "Subject": true, "Date": true, "Message-Id": true,
		"In-Reply-To": true, "References": true, "Mime-Version": true, "Content-Type": true, "Content-Transfer-Encoding": true}

	f.Fuzz(func(t *testing.T, messageID, references, inReplyTo string) {
		irt, refs := ReplyThreading(messageID, references, inReplyTo)
		if irt == "" {
			if refs != nil {
				t.Fatalf("refs without In-Reply-To: %q", refs)
			}
			return
		}
		if len(refs) == 0 || len(refs) > maxReferences || refs[len(refs)-1] != irt {
			t.Fatalf("bad references for %q: %q", irt, refs)
		}
		for _, id := range append([]string{irt}, refs...) {
			if !wholeMsgID.MatchString(id) {
				t.Fatalf("malformed msg-id %q", id)
			}
		}

		raw := Message{From: "a@x.test", To: []string{"b@x.test"}, Subject: "Re", Body: "b",
			MessageID: "<m@x.test>", InReplyTo: irt, References: refs}.Build()
		head, _, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
		for line := range strings.SplitSeq(string(head), "\r\n") {
			if len(line) > 998 {
				t.Fatalf("header line of %d octets", len(line))
			}
		}
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("reply does not parse: %v", err)
		}
		for k := range msg.Header {
			if !want[k] {
				t.Fatalf("unexpected header %q", k)
			}
		}
		if got := msg.Header.Get("In-Reply-To"); got != irt {
			t.Fatalf("In-Reply-To = %q, want %q", got, irt)
		}
	})
}
