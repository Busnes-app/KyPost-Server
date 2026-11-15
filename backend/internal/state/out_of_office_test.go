package state

import (
	"context"
	"fmt"
	"testing"
)

func TestClaimOutOfOfficeReply(t *testing.T) {
	s := newSnoozeStore(t)
	ctx := context.Background()
	claim := func(key, sender, period string, at int64) bool {
		t.Helper()
		ok, err := s.ClaimOutOfOfficeReply(ctx, key, sender, period, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !claim("m1", "a@x", "p1", 1000) {
		t.Fatal("first reply refused")
	}
	if claim("m1", "b@x", "p2", 1001) {
		t.Fatal("a message was answered twice")
	}
	if claim("m2", "a@x", "p1", 1002) {
		t.Fatal("same sender answered twice in one period")
	}
	if !claim("m3", "a@x", "p2", 1003) {
		t.Fatal("a new period did not answer again")
	}
	if claim("m4", "a@x", "p2", 1003+OOOSenderWindow-1) {
		t.Fatal("answered again inside the window")
	}
	if !claim("m5", "a@x", "p2", 1003+OOOSenderWindow) {
		t.Fatal("not answered again after the window")
	}
}

func TestClaimOutOfOfficeReplyFullTable(t *testing.T) {
	s := newSnoozeStore(t)
	ctx := context.Background()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxOOOSenders {
		if _, err := tx.Exec(`INSERT INTO ooo_senders(sender, period, at) VALUES(?, 'p', 100)`, fmt.Sprintf("s%d@x", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimOutOfOfficeReply(ctx, "m", "new@x", "p", 200); err != nil || ok {
		t.Fatalf("full table claimed a new sender: %v %v", ok, err)
	}
	if ok, err := s.ClaimOutOfOfficeReply(ctx, "m2", "s1@x", "q", 200); err != nil || !ok {
		t.Fatalf("full table refused a known sender's new period: %v %v", ok, err)
	}
}
