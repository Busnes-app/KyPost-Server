//go:build linux

package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldClient opens a prepared account's client whose admission is only the
// held storage check, as sso runs it for the client's own mailbox.
func heldClient(t testing.TB) (c *Client, account string, admitAs *Owner) {
	t.Helper()
	root := t.TempDir()
	owner, limits := preparationOwner(), preparationLimits()
	source, err := PrepareAccount(root, owner, "one@example.test", limits)
	if err != nil {
		t.Fatal(err)
	}
	admitAs = &owner
	c, err = OpenClient(filepath.Join(root, "users", owner.Mailbox, "mailbox"), owner, limits, "one@example.test", source, func(_ context.Context, held *Prepared) error {
		got, err := held.ValidatePreparedMailbox(filepath.Join(root, "users"), *admitAs, "one@example.test", limits)
		if err == nil && got != source {
			err = ErrPreparation
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, filepath.Join(root, "users", owner.Mailbox), admitAs
}

// replaceWithCopy renames a byte-identical copy over path: a different file
// that every content check accepts.
func replaceWithCopy(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path+".copy", raw, 0o600))
	must(t, os.Rename(path+".copy", path))
}

func rewritePreparation(t *testing.T, account string, edit func(*preparedAccount)) {
	t.Helper()
	path := filepath.Join(account, preparationFile)
	var p preparedAccount
	raw, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(raw, &p))
	edit(&p)
	raw, err = json.Marshal(p)
	must(t, err)
	must(t, os.WriteFile(path, raw, 0o600))
}

// Each mutation lands while the client is open; the next operation refuses.
// Copies are valid storage the full, reopening check accepts, so only the
// held handles' file identity refuses them.
func TestHeldAdmissionRefusesChangedStorage(t *testing.T) {
	limits := preparationLimits()
	for name, tc := range map[string]struct {
		mutate     func(t *testing.T, account string, admitAs *Owner)
		fullAccept bool
	}{
		"account deleted":   {mutate: func(t *testing.T, account string, _ *Owner) { must(t, os.RemoveAll(account)) }},
		"state.db replaced": {mutate: func(t *testing.T, account string, _ *Owner) { replaceWithCopy(t, filepath.Join(account, "state.db")) }, fullAccept: true},
		"mailbox.db replaced": {mutate: func(t *testing.T, account string, _ *Owner) {
			replaceWithCopy(t, filepath.Join(account, "mailbox", "mailbox.db"))
		}, fullAccept: true},
		"state.db deleted": {mutate: func(t *testing.T, account string, _ *Owner) { must(t, os.Remove(filepath.Join(account, "state.db"))) }},
		"account mode":     {mutate: func(t *testing.T, account string, _ *Owner) { must(t, os.Chmod(account, 0o750)) }},
		"mailbox directory mode": {mutate: func(t *testing.T, account string, _ *Owner) {
			must(t, os.Chmod(filepath.Join(account, "mailbox"), 0o705))
		}},
		"preparation changed": {mutate: func(t *testing.T, account string, _ *Owner) {
			rewritePreparation(t, account, func(p *preparedAccount) { p.Address = "two@example.test" })
		}},
		"preparation source changed": {mutate: func(t *testing.T, account string, _ *Owner) {
			rewritePreparation(t, account, func(p *preparedAccount) { p.Source = "native:other" })
		}},
		"state source changed": {mutate: func(t *testing.T, account string, _ *Owner) {
			db, err := openSQLite(filepath.Join(account, "state.db"), "mode=rw")
			must(t, err)
			defer db.Close()
			_, err = db.Exec("UPDATE meta SET value='native:other' WHERE key='mail_source'")
			must(t, err)
		}},
		"identity changed": {mutate: func(t *testing.T, account string, _ *Owner) {
			db, err := openSQLite(filepath.Join(account, "mailbox", "mailbox.db"), "mode=rw")
			must(t, err)
			defer db.Close()
			_, err = db.Exec("UPDATE identity SET subject='subject-two'")
			must(t, err)
		}},
		"identity limits changed": {mutate: func(t *testing.T, account string, _ *Owner) {
			db, err := openSQLite(filepath.Join(account, "mailbox", "mailbox.db"), "mode=rw")
			must(t, err)
			defer db.Close()
			_, err = db.Exec("UPDATE identity SET records=99")
			must(t, err)
		}},
		"owner mismatch": {mutate: func(_ *testing.T, _ string, admitAs *Owner) { admitAs.Subject = "subject-two" }},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, account, admitAs := heldClient(t)
			if _, err := c.ListLabels(ctx); err != nil {
				t.Fatal("intact storage refused:", err)
			}
			tc.mutate(t, account, admitAs)
			if _, err := ValidatePreparedMailbox(filepath.Dir(account), *admitAs, "one@example.test", limits); (err == nil) != tc.fullAccept {
				t.Fatalf("full check: accept %v, want %v", err == nil, tc.fullAccept)
			}
			if _, err := c.ListLabels(ctx); !errors.Is(err, ErrPreparation) {
				t.Fatalf("open client accepted %s: %v", name, err)
			}
		})
	}
}

// BenchmarkAdmissionStorage compares the per-operation storage check: the
// full path reopens both databases, the held path reuses the client's.
// The mailbox holds 2,000 messages so the schemas and files are realistic.
func BenchmarkAdmissionStorage(b *testing.B) {
	root := b.TempDir()
	owner, limits := preparationOwner(), Limits{MessageBytes: 1 << 20, PayloadBytes: 1 << 30, Records: 100000}
	source, err := PrepareAccount(root, owner, "one@example.test", limits)
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(root, "users", owner.Mailbox, "mailbox")
	s, err := OpenExisting(dir, owner, limits, source)
	if err != nil {
		b.Fatal(err)
	}
	for i := range 2000 {
		if _, err = s.Append(context.Background(), "INBOX", strings.NewReader(fmt.Sprintf("From: s@example.test\r\nTo: one@example.test\r\nSubject: %d\r\n\r\n%s", i, strings.Repeat("body ", 800))), false); err != nil {
			b.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		b.Fatal(err)
	}
	parent := filepath.Join(root, "users")
	b.Run("full", func(b *testing.B) {
		for b.Loop() {
			if _, err := ValidatePreparedMailbox(parent, owner, "one@example.test", limits); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("held", func(b *testing.B) {
		held := &Prepared{dir: filepath.Join(parent, owner.Mailbox)}
		defer held.Close()
		if _, err := held.check(owner, "one@example.test", limits); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if _, err := held.ValidatePreparedMailbox(parent, owner, "one@example.test", limits); err != nil {
				b.Fatal(err)
			}
		}
	})
}
