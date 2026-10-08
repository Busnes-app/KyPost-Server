//go:build linux

package mailbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// ConvergeLimits brings a published mailbox to new limits; a crash between
// the database and the preparation file is completed by the next run, and a
// different target after that crash still converges.
func TestConvergeLimitsCrashAndRetarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	owner, old := preparationOwner(), preparationLimits()
	source, err := PrepareAccount(root, owner, "one@example.test", old)
	must(t, err)
	dir := filepath.Join(root, "users", owner.Mailbox)
	raised := Limits{2 << 20, 64 << 20, 1000}
	crash := errors.New("crash")
	convergeStep = func() error { return crash }
	if _, err := ConvergeLimits(ctx, dir, owner, raised); !errors.Is(err, crash) {
		t.Fatalf("crash not reached: %v", err)
	}
	convergeStep = func() error { return nil }
	if _, err := ValidatePreparedAccount(root, owner, "one@example.test", raised); !errors.Is(err, ErrPreparation) {
		t.Fatalf("half-converged mailbox validated: %v", err)
	}
	for _, target := range []Limits{raised, {2 << 20, 128 << 20, 1000}} {
		for want := true; ; want = false {
			wrote, err := ConvergeLimits(ctx, dir, owner, target)
			must(t, err)
			if wrote != want {
				t.Fatalf("%+v: wrote=%v, want %v", target, wrote, want)
			}
			if !want {
				break
			}
		}
		if got, err := ValidatePreparedAccount(root, owner, "one@example.test", target); err != nil || got != source {
			t.Fatalf("converged mailbox refused: %v", err)
		}
		s, err := OpenExisting(filepath.Join(dir, "mailbox"), owner, target, source)
		must(t, err)
		must(t, s.Close())
	}
	foreign := owner
	foreign.Subject = "other"
	if _, err := ConvergeLimits(ctx, dir, foreign, raised); !errors.Is(err, ErrPreparation) {
		t.Fatalf("foreign owner: %v", err)
	}
	if wrote, err := ConvergeLimits(ctx, filepath.Join(root, "users", "never"), Owner{owner.Issuer, owner.Subject, "never"}, raised); wrote || err != nil {
		t.Fatalf("unpublished mailbox: %v %v", wrote, err)
	}
}

// A quota lowered below what a mailbox stores refuses new mail at the quota
// and keeps the mailbox open, readable and its mail deletable.
func TestQuotaRefusesAtLimitAndLoweringKeepsMail(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	msg := "Subject: x\r\n\r\n" + strings.Repeat("x", 100) + "\r\n"
	size := int64(len(msg))
	limits := Limits{MessageBytes: size, PayloadBytes: 2 * size, Records: 100}
	s := openTest(t, dir, testOwner, limits)
	for range 2 {
		if _, err := s.Append(ctx, "INBOX", strings.NewReader(msg), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(ctx, "INBOX", strings.NewReader(msg), false); !errors.Is(err, ErrCapacity) {
		t.Fatalf("at the quota: %v", err)
	}
	u, err := ReadUsage(dir)
	if err != nil || u != (Usage{Bytes: 2 * size, Records: 2}) || u.Fits(limits, 1) || !u.Fits(Limits{size, 3 * size, 100}, size) || u.Fits(Limits{size, 3 * size, 2}, 1) {
		t.Fatalf("usage %+v %v", u, err)
	}
	must(t, s.Close())
	lowered := Limits{MessageBytes: size, PayloadBytes: size, Records: 100}
	db, err := openSQLite(filepath.Join(dir, "mailbox.db"), "mode=rw")
	must(t, err)
	_, err = db.Exec("UPDATE identity SET payload_bytes=?", lowered.PayloadBytes)
	must(t, err)
	must(t, db.Close())
	s = openTest(t, dir, testOwner, lowered)
	defer s.Close()
	page, err := s.List(ctx, "INBOX", 0, 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("over-quota mailbox unreadable: %d %v", len(page), err)
	}
	if _, err := s.Append(ctx, "INBOX", strings.NewReader("Subject: y\r\n\r\ny\r\n"), false); !errors.Is(err, ErrCapacity) {
		t.Fatalf("over quota accepted mail: %v", err)
	}
	must(t, s.Delete(ctx, "INBOX", page[0].ID))
	must(t, s.Delete(ctx, "INBOX", page[1].ID))
	if _, err := s.Append(ctx, "INBOX", strings.NewReader("Subject: y\r\n\r\ny\r\n"), false); err != nil {
		t.Fatalf("freed mailbox refused mail: %v", err)
	}
}
