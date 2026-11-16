package config

import (
	"strconv"
	"testing"
)

// Answers may come back in any order: an older applied answer never forgets a
// text a newer sync carried, and a newer one forgets what only older ones did.
func TestOOORemoteTextsSurviveReorderedAnswers(t *testing.T) {
	var l []OOORemoteText
	l, _ = NoteOOORemoteSent(l, "T", 1) // synced earlier
	l, _ = NoteOOORemoteSent(l, "U", 2) // sync A
	l, _ = NoteOOORemoteSent(l, "T", 3) // sync B reuses T
	l = ForgetOOORemoteBefore(l, 3)     // B applied first
	l = ForgetOOORemoteBefore(l, 2)     // A's answer arrives late
	if len(l) != 1 || l[0] != (OOORemoteText{Text: "T", Revision: 3}) {
		t.Fatalf("kept %v", l)
	}
	for i := range MaxOOORemoteTexts - 1 {
		var ok bool
		if l, ok = NoteOOORemoteSent(l, strconv.Itoa(i), 4); !ok {
			t.Fatalf("refused at %d", i)
		}
	}
	if _, ok := NoteOOORemoteSent(l, "one more", 5); ok {
		t.Fatal("a full list took a new text")
	}
	if _, ok := NoteOOORemoteSent(l, "T", 5); !ok {
		t.Fatal("a full list refused a text it holds")
	}
}
