package config

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

// Answers may come back in any order: an older applied answer never forgets a
// text a newer sync carried, and a newer one forgets what only older ones did.
func TestOOORemoteTextsSurviveReorderedAnswers(t *testing.T) {
	var l []OOORemoteText
	l, _ = NoteOOORemoteSent(l, "T", 1) // synced earlier
	l, _ = NoteOOORemoteSent(l, "U", 2) // sync A
	l, _ = NoteOOORemoteSent(l, "T", 3) // sync B reuses T
	now := time.Now()
	l = ForgetOOORemoteBefore(l, 3, now) // B applied first
	l = ForgetOOORemoteBefore(l, 2, now) // A's answer arrives late
	if len(l) != 2 || l[0] != (OOORemoteText{Text: "T", Revision: 3}) || l[1].Superseded == 0 {
		t.Fatalf("kept %v", l)
	}
	// U stays refused while a queued REPLY may carry it, then is forgotten.
	if got := ForgetOOORemoteBefore(slices.Clone(l), 3, now.Add(OOORemoteRetention)); len(got) != 2 {
		t.Fatalf("forgotten too early: %v", got)
	}
	if l = ForgetOOORemoteBefore(l, 3, now.Add(OOORemoteRetention+time.Second)); len(l) != 1 || l[0].Text != "T" {
		t.Fatalf("after retention: %v", l)
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
