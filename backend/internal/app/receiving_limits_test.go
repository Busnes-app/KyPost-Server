//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// A full mailbox refuses at RCPT and at DATA with exit 9 (452 4.2.2) and
// stores nothing; others still receive. Growth inside the drive reserve exits
// 10 (452 4.3.1) from both commands. Both are temporary: senders retry.
func TestNativeReceivingQuotaAndDriveReserve(t *testing.T) {
	r, created := receivingFixture(t)
	ctx := context.Background()
	for _, address := range []string{"one@example.test", "two@example.test"} {
		if err := r.bind(ctx, "raced", "", address); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := r.life.NativeAssignment(created[0].NativeMailboxIssuer, created[0].SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", created[0].ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("From: test@outside.test\r\nSubject: quota filler\r\n\r\n")
	for used := int64(0); used < a.Limits.PayloadBytes; {
		size := min(a.Limits.MessageBytes, a.Limits.PayloadBytes-used)
		if _, err := store.Append(ctx, "INBOX", bytes.NewReader(append(prefix, bytes.Repeat([]byte("x"), int(size)-len(prefix))...)), false); err != nil {
			t.Fatal(err)
		}
		used += size
	}
	_ = store.Close()
	if err := r.bind(ctx, "full", "", "one@example.test"); receivingExit(err) != 9 || !errors.Is(err, errMailboxFull) {
		t.Fatalf("RCPT to a full mailbox: %v", err)
	}
	if err := r.accept(ctx, "raced", "", bytes.NewReader([]byte("From: a@outside.test\r\n\r\nraced\r\n"))); receivingExit(err) != 9 {
		t.Fatalf("DATA to a full mailbox: %v", err)
	}
	if d, err := r.holding.Get(ctx, receivingGateway, "raced"); err != nil || d.State != "staged" || len(d.Raw) != 0 {
		t.Fatalf("refused DATA stored bytes: %+v %v", d, err)
	}
	if err := r.bind(ctx, "other", "", "two@example.test"); err != nil {
		t.Fatalf("a full mailbox blocked another: %v", err)
	}

	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 10 << 30, 100 << 30, nil }
	if err := r.bind(ctx, "reserve", "", "two@example.test"); receivingExit(err) != 10 || !errors.Is(err, fsutil.ErrDriveReserve) {
		t.Fatalf("RCPT inside the reserve: %v", err)
	}
	if err := r.accept(ctx, "other", "", bytes.NewReader([]byte("From: a@outside.test\r\n\r\nreserve\r\n"))); receivingExit(err) != 10 {
		t.Fatalf("DATA inside the reserve: %v", err)
	}
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 11 << 30, 100 << 30, nil }
	if err := r.accept(ctx, "other", "", bytes.NewReader([]byte("From: a@outside.test\r\n\r\nreserve\r\n"))); err != nil {
		t.Fatalf("DATA above the reserve: %v", err)
	}
	if receivingExit(errMailboxMessageLimit) != 1 || receivingExit(&receivingCommandError{err: errMailboxFull, code: 9}) != 9 {
		t.Fatal("exit mapping")
	}
}

// Hosted pickup: mail for a full mailbox waits in R2 (neither imported nor
// quarantined) while others' mail flows; inside the drive reserve fetching stops.
func TestCloudflareContinuousQuotaAndDriveReserve(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	u := e.users[0]
	a, _, err := e.r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(e.r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("From: test@outside.test\r\nSubject: quota filler\r\n\r\n")
	for used := int64(0); used < a.Limits.PayloadBytes; {
		size := min(a.Limits.MessageBytes, a.Limits.PayloadBytes-used)
		if _, err := store.Append(ctx, "Archive", bytes.NewReader(append(prefix, bytes.Repeat([]byte("x"), int(size)-len(prefix))...)), false); err != nil {
			t.Fatal(err)
		}
		used += size
	}
	_ = store.Close()
	full := e.w.capture("one@example.test", "a@outside.test", "full", nil)
	e.w.capture("two@example.test", "a@outside.test", "fine", nil)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[1], "INBOX"); len(got) != 1 || got[0] != "fine" {
		t.Fatalf("a full mailbox held up another: %v", got)
	}
	if _, kept := e.w.object(full); !kept || len(cfFolder(t, e.r, u, "INBOX")) != 0 {
		t.Fatal("mail for a full mailbox left R2 or was stored")
	}
	if d, err := e.r.holding.Get(ctx, cfGateway, full); err == nil && d.State == "quarantined" {
		t.Fatal("mail for a full mailbox quarantined")
	}
	if s := cfStatus(t, e); s.State == "error" {
		t.Fatalf("a full mailbox stopped pickup: %+v", s)
	}

	defer func(orig func(string) (uint64, uint64, error)) { fsutil.DiskSpace = orig }(fsutil.DiskSpace)
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 10 << 30, 100 << 30, nil }
	e.clock.advance(time.Hour)
	waits := e.w.capture("two@example.test", "a@outside.test", "waits", nil)
	e.cycle(t)
	if _, kept := e.w.object(waits); !kept || len(cfFolder(t, e.r, e.users[1], "INBOX")) != 1 {
		t.Fatal("mail taken inside the drive reserve")
	}
}
