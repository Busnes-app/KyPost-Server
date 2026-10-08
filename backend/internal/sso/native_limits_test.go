//go:build linux

package sso

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// MigrateNativeLimits raises a primary and an extra mailbox in the ledger,
// mailbox.db and native-mailbox.json; a crash after the ledger write, or
// between mailboxes, completes on the next run, a second run is a no-op and a
// restored copy with the old limits migrates again.
// A quota then lowered below a mailbox's usage keeps it admitted and readable.
func TestMigrateNativeLimitsRaisesConvergesAndLowers(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	one := f.ids["one"]
	extra, err := f.life.CreateNativeMailbox(ctx, f.root, one, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	primary, _, err := f.life.NativeMailboxAssignment(one)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(primary.Dir(f.root), "mailbox"), primary.Owner, nativeLimits, primary.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Append(ctx, "INBOX", strings.NewReader("Subject: kept\r\n\r\n"+strings.Repeat("x", 4096)+"\r\n"), false); err != nil {
		t.Fatal(err)
	}
	box.Close()
	admitted := func(limits mailbox.Limits) error {
		for _, id := range []string{one, extra.ID} {
			a, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, one, id, f.accounts)
			if err != nil {
				return fmt.Errorf("admit %s: %w", id, err)
			}
			if a.Limits != limits {
				return errors.New("ledger limits not migrated")
			}
			s, err := mailbox.OpenExisting(filepath.Join(a.Dir(f.root), "mailbox"), a.Owner, a.Limits, a.Source)
			if err != nil {
				return fmt.Errorf("open %s: %w", id, err)
			}
			s.Close()
		}
		return nil
	}
	raised := mailbox.NativeLimits(8 << 20)
	raised.MessageBytes = 2 << 20
	// An offline copy with the old limits, restored later like an old backup.
	backup := t.TempDir()
	copyTree := func(from, to string) {
		t.Helper()
		if out, err := exec.Command("cp", "-a", from+"/.", to).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	for _, dir := range []string{"config", "root"} {
		if err := os.Mkdir(filepath.Join(backup, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyTree(f.config, filepath.Join(backup, "config"))
	copyTree(f.root, filepath.Join(backup, "root"))

	crash := errors.New("crash")
	nativeLimitsStep = func() error { return crash }
	if _, err := MigrateNativeLimits(ctx, f.config, raised); !errors.Is(err, crash) {
		t.Fatalf("crash not reached: %v", err)
	}
	nativeLimitsStep = func() error { return nil }
	if err := admitted(raised); err == nil {
		t.Fatal("mailboxes admitted before their files converged")
	}
	// Another crash after only the extra mailbox converged.
	if _, err := mailbox.ConvergeLimits(ctx, filepath.Join(f.root, nativeMailboxesDir, extra.ID), mailbox.Owner{Issuer: nativeIssuer, Subject: "one", Mailbox: extra.ID}, raised); err != nil {
		t.Fatal(err)
	}
	if migrated, err := MigrateNativeLimits(ctx, f.config, raised); err != nil || !migrated {
		t.Fatalf("resumed migration: %v %v", migrated, err)
	}
	if err := admitted(raised); err != nil {
		t.Fatalf("migrated mailboxes refused: %v", err)
	}
	ledger := mustRead(t, filepath.Join(f.config, nativeProvisioningFile))
	if migrated, err := MigrateNativeLimits(ctx, f.config, raised); err != nil || migrated || !bytes.Equal(ledger, mustRead(t, filepath.Join(f.config, nativeProvisioningFile))) {
		t.Fatalf("second run changed state: %v %v", migrated, err)
	}
	for from, to := range map[string]string{"config": f.config, "root": f.root} {
		if err := os.RemoveAll(to); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(to, 0o700); err != nil {
			t.Fatal(err)
		}
		copyTree(filepath.Join(backup, from), to)
	}
	if err := admitted(raised); err == nil {
		t.Fatal("restored old limits admitted under the new ones")
	}
	if migrated, err := MigrateNativeLimits(ctx, f.config, raised); err != nil || !migrated {
		t.Fatalf("restored backup not migrated: %v %v", migrated, err)
	}
	if err := admitted(raised); err != nil {
		t.Fatalf("restored backup refused after migration: %v", err)
	}
	mailboxes, err := f.life.NativeMailboxes()
	if err != nil || len(mailboxes) != 2 || mailboxes[0].QuotaBytes != raised.PayloadBytes || mailboxes[1].QuotaBytes != raised.PayloadBytes {
		t.Fatalf("listed quota: %+v %v", mailboxes, err)
	}

	lowered := raised
	lowered.MessageBytes = 1024
	lowered.PayloadBytes = 1024
	if _, err := MigrateNativeLimits(ctx, f.config, lowered); err != nil {
		t.Fatal(err)
	}
	if err := admitted(lowered); err != nil {
		t.Fatalf("over-quota mailbox locked: %v", err)
	}
	s, err := mailbox.OpenExisting(filepath.Join(primary.Dir(f.root), "mailbox"), primary.Owner, lowered, primary.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if page, err := s.List(ctx, "INBOX", 0, 10); err != nil || len(page) != 1 {
		t.Fatalf("kept mail: %d %v", len(page), err)
	}
	if _, err := s.Append(ctx, "INBOX", strings.NewReader("Subject: new\r\n\r\nx\r\n"), false); !errors.Is(err, mailbox.ErrCapacity) {
		t.Fatalf("over-quota mailbox took mail: %v", err)
	}
}

// One mailbox that cannot take the limits is named; the others converge.
func TestMigrateNativeLimitsNamesTheFailedMailbox(t *testing.T) {
	ctx := context.Background()
	f := newAddressFixture(t, "one")
	one := f.ids["one"]
	extra, err := f.life.CreateNativeMailbox(ctx, f.root, one, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(f.root, nativeMailboxesDir, extra.ID, "native-mailbox.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	raised := mailbox.NativeLimits(8 << 20)
	raised.MessageBytes = 2 << 20
	_, err = MigrateNativeLimits(ctx, f.config, raised)
	if !errors.Is(err, ErrNativeMailboxLimits) || !strings.Contains(err.Error(), extra.ID) || strings.Contains(err.Error(), "mailbox "+one) {
		t.Fatalf("failure report: %v", err)
	}
	a, err := f.life.AdmitNativeMailbox(ctx, f.root, nativeIssuer, one, one, f.accounts)
	if err != nil || a.Limits != raised {
		t.Fatalf("the healthy primary was not migrated: %v", err)
	}
}
