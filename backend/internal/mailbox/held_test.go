//go:build linux

package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// pooledAccount prepares an account and holds its handles in this process's
// admission pool (bound max), as the api and daemon do.
func pooledAccount(t *testing.T, max int) (account string, admit func() (string, error)) {
	t.Helper()
	HoldAdmissionHandles(max)
	t.Cleanup(func() { HoldAdmissionHandles(0) })
	root := t.TempDir()
	owner, limits := preparationOwner(), preparationLimits()
	source, err := PrepareAccount(root, owner, "one@example.test", limits)
	must(t, err)
	admit = func() (string, error) {
		got, err := (*Prepared)(nil).ValidatePreparedMailbox(filepath.Join(root, "users"), owner, "one@example.test", limits)
		if err == nil && got != source {
			err = ErrPreparation
		}
		return got, err
	}
	if _, err = admit(); err != nil {
		t.Fatal("intact storage refused:", err)
	}
	return filepath.Join(root, "users", owner.Mailbox), admit
}

func heldCount() int {
	heldAdmissions.Lock()
	defer heldAdmissions.Unlock()
	return len(heldAdmissions.m)
}

// copyAccount makes a byte-identical copy of the account directory, with
// its modes, at dst: storage the full check accepts.
func copyAccount(t *testing.T, src, dst string) {
	t.Helper()
	must(t, os.CopyFS(dst, os.DirFS(src)))
	must(t, filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	}))
}

// A held admission handle sees every change the reopening check sees: each
// file replaced by a valid copy, the directory retargeted to a valid copy,
// a schema change and a content change in place. A refusal drops the
// handles, so storage that is valid again (the copies) admits by reopening.
func TestPooledAdmissionRefusesChangedStorage(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(t *testing.T, account string)
		recover bool // the change is itself valid storage
	}{
		"state.db replaced":   {mutate: func(t *testing.T, a string) { replaceWithCopy(t, filepath.Join(a, "state.db")) }, recover: true},
		"mailbox.db replaced": {mutate: func(t *testing.T, a string) { replaceWithCopy(t, filepath.Join(a, "mailbox", "mailbox.db")) }, recover: true},
		"directory retargeted": {mutate: func(t *testing.T, a string) {
			copyAccount(t, a, a+".copy")
			must(t, os.Rename(a, a+".old"))
			must(t, os.Rename(a+".copy", a))
		}, recover: true},
		"account deleted": {mutate: func(t *testing.T, a string) { must(t, os.RemoveAll(a)) }},
		"mailbox.db gone": {mutate: func(t *testing.T, a string) { must(t, os.Remove(filepath.Join(a, "mailbox", "mailbox.db"))) }},
		"preparation file": {mutate: func(t *testing.T, a string) {
			rewritePreparation(t, a, func(p *preparedAccount) { p.Source = "native:other" })
		}},
		"state table dropped": {mutate: func(t *testing.T, a string) {
			db, err := openSQLite(filepath.Join(a, "state.db"), "mode=rw")
			must(t, err)
			defer db.Close()
			_, err = db.Exec("DROP TABLE sorter_corrections")
			must(t, err)
		}},
		"identity changed": {mutate: func(t *testing.T, a string) {
			db, err := openSQLite(filepath.Join(a, "mailbox", "mailbox.db"), "mode=rw")
			must(t, err)
			defer db.Close()
			_, err = db.Exec("UPDATE identity SET subject='subject-two'")
			must(t, err)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			account, admit := pooledAccount(t, MaxHeldAdmissions)
			if heldCount() != 1 {
				t.Fatal("admission did not hold its handles")
			}
			tc.mutate(t, account)
			if _, err := admit(); !errors.Is(err, ErrPreparation) {
				t.Fatalf("held handle accepted %s: %v", name, err)
			}
			if heldCount() != 0 {
				t.Fatal("a refused handle was kept")
			}
			if _, err := admit(); (err == nil) != tc.recover {
				t.Fatalf("after reopening: accept %v, want %v", err == nil, tc.recover)
			}
		})
	}
}

// Another process changes the mailbox while this one holds its handles:
// the next admission reads the committed change or refuses the replacement.
func TestPooledAdmissionSeesOtherProcess(t *testing.T) {
	for _, change := range []string{"identity", "replace-state", "replace-mailbox"} {
		t.Run(change, func(t *testing.T) {
			account, admit := pooledAccount(t, MaxHeldAdmissions)
			command := exec.Command(os.Args[0], "-test.run=^TestPooledAdmissionWriterProcess$")
			command.Env = append(os.Environ(), "KYPOST_MAILBOX_CHILD="+change, "KYPOST_MAILBOX_ACCOUNT="+account)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("independent writer failed: %v\n%s", err, output)
			}
			if _, err := admit(); !errors.Is(err, ErrPreparation) {
				t.Fatal("held handle missed another process's change", err)
			}
		})
	}
}

func TestPooledAdmissionWriterProcess(t *testing.T) {
	account := os.Getenv("KYPOST_MAILBOX_ACCOUNT")
	switch os.Getenv("KYPOST_MAILBOX_CHILD") {
	case "identity":
		db, err := openSQLite(filepath.Join(account, "mailbox", "mailbox.db"), "mode=rw")
		must(t, err)
		defer db.Close()
		_, err = db.Exec("UPDATE identity SET records=99")
		must(t, err)
	case "replace-state":
		replaceWithCopy(t, filepath.Join(account, "state.db"))
	case "replace-mailbox":
		replaceWithCopy(t, filepath.Join(account, "mailbox", "mailbox.db"))
	}
}

// The bound, the single entry per mailbox and the idle close.
func TestPooledAdmissionBounds(t *testing.T) {
	account, admit := pooledAccount(t, 1)
	other := filepath.Join(filepath.Dir(account), "other")
	copyAccount(t, account, other)
	if _, err := checkHeld(other, preparationOwner(), "one@example.test", preparationLimits()); err != nil {
		t.Fatal(err)
	}
	if heldCount() != 1 || heldAdmissions.m[other] != nil {
		t.Fatal("held past the bound")
	}
	HoldAdmissionHandles(2)
	_, err := admit()
	must(t, err)
	heldAdmissions.Lock()
	p := heldAdmissions.m[account]
	heldAdmissions.Unlock()
	if holdAdmission(&Prepared{dir: account}) || heldAdmissions.m[account] != p {
		t.Fatal("a second handle replaced the held one")
	}
	_, err = checkHeld(other, preparationOwner(), "one@example.test", preparationLimits())
	must(t, err)
	heldAdmissions.Lock()
	idle := heldAdmissions.m[other]
	idle.used = time.Now().Add(-heldIdle)
	heldAdmissions.Unlock()
	closeIdleAdmissions()
	if heldCount() != 1 || heldAdmissions.m[account] == nil || idle.state.Ping() == nil {
		t.Fatal("idle close missed the idle handle or closed the used one")
	}
}
