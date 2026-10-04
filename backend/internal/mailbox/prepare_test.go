//go:build linux

package mailbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func preparationOwner() Owner {
	return Owner{"https://identity.example.test", "subject-one", "account-one"}
}
func preparationLimits() Limits { return Limits{1 << 20, 4 << 20, 100} }

func TestPrepareAccountReferenceGenerationReadOnly(t *testing.T) {
	root := t.TempDir()
	owner, limits := preparationOwner(), preparationLimits()
	source, err := PrepareAccount(root, owner, "one@example.test", limits)
	must(t, err)
	dir := filepath.Join(root, "users", owner.Mailbox, "mailbox")
	s, err := OpenExisting(dir, owner, limits, source)
	must(t, err)
	defer s.Close()
	_, err = s.db.Exec("DROP TABLE reference_generation")
	must(t, err)
	_, err = ValidatePreparedAccount(root, owner, "one@example.test", limits)
	must(t, err)
	var tables int
	must(t, s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='reference_generation'").Scan(&tables))
	if tables != 0 {
		t.Fatal("historical validation mutated older schema")
	}
	must(t, s.Close())
	s, err = OpenExisting(dir, owner, limits, source)
	must(t, err)
	defer s.Close()
	if s.MessageReferenceGeneration() == "" {
		t.Fatal("runtime older-schema migration failed")
	}
	_, err = s.db.Exec("DELETE FROM reference_generation")
	must(t, err)
	if _, err = ValidatePreparedAccount(root, owner, "one@example.test", limits); !errors.Is(err, ErrPreparation) {
		t.Fatal("prepared corruption accepted", err)
	}
	var rows int
	must(t, s.db.QueryRow("SELECT count(*) FROM reference_generation").Scan(&rows))
	if rows != 0 {
		t.Fatal("validation repaired corruption")
	}
}

func TestPrepareAccountRetriesPreserveMail(t *testing.T) {
	root := t.TempDir()
	owner := preparationOwner()
	limits := preparationLimits()
	first, err := PrepareAccount(root, owner, "one@example.test", limits)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "users", owner.Mailbox)
	st, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.BindMailSource(first); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	store, err := Open(filepath.Join(dir, "mailbox"), owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\nSubject: retained\r\n\r\nexact body")
	id, err := store.Append(context.Background(), "INBOX", bytes.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	for i := 0; i < 2; i++ {
		next, err := PrepareAccount(root, owner, "one@example.test", limits)
		if err != nil || next != first {
			t.Fatalf("retry changed source: %s %v", next, err)
		}
	}
	for _, tc := range []struct {
		owner   Owner
		address string
		limits  Limits
	}{
		{Owner{"https://other.example.test", owner.Subject, owner.Mailbox}, "one@example.test", limits},
		{Owner{owner.Issuer, "new-subject", owner.Mailbox}, "one@example.test", limits},
		{owner, "two@example.test", limits},
		{owner, "one@example.test", Limits{1 << 20, 5 << 20, 100}},
	} {
		if _, err = PrepareAccount(root, tc.owner, tc.address, tc.limits); !errors.Is(err, ErrPreparation) {
			t.Fatal("existing preparation changed", err)
		}
	}
	store, err = Open(filepath.Join(dir, "mailbox"), owner, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Raw(context.Background(), "INBOX", id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("retry/refusal changed mail", err)
	}
}

func TestPrepareAccountRefusesIncompleteOrLegacy(t *testing.T) {
	for _, broken := range []string{"legacy", "state.db", "mailbox/mailbox.db", "native-mailbox.json", "corrupt-mailbox", "corrupt-state", "empty-mailbox"} {
		t.Run(broken, func(t *testing.T) {
			root := t.TempDir()
			owner := preparationOwner()
			dir := filepath.Join(root, "users", owner.Mailbox)
			if broken == "legacy" {
				st, err := state.New(dir)
				if err != nil {
					t.Fatal(err)
				}
				_ = st.Close()
			} else {
				if _, err := PrepareAccount(root, owner, "one@example.test", preparationLimits()); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, broken)
				switch broken {
				case "corrupt-mailbox", "empty-mailbox":
					path = filepath.Join(dir, "mailbox", "mailbox.db")
				case "corrupt-state":
					path = filepath.Join(dir, "state.db")
				}
				if broken == "corrupt-mailbox" || broken == "corrupt-state" || broken == "empty-mailbox" {
					data := []byte("invalid database")
					if broken == "empty-mailbox" {
						data = nil
					}
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string][]byte{}
			for _, name := range []string{"state.db", "mailbox/mailbox.db", "native-mailbox.json"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err == nil {
					files[name] = data
				}
			}
			if _, err := PrepareAccount(root, owner, "one@example.test", preparationLimits()); err == nil {
				t.Fatal("incomplete account adopted")
			}
			for _, name := range []string{"state.db", "mailbox/mailbox.db", "native-mailbox.json"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				before, exists := files[name]
				if exists {
					if err != nil || !bytes.Equal(data, before) {
						t.Fatal("refusal modified " + name)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refusal recreated " + name)
				}
			}
		})
	}
}

func TestPrepareAccountConcurrent(t *testing.T) {
	root := t.TempDir()
	results := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			source, err := PrepareAccount(root, preparationOwner(), "one@example.test", preparationLimits())
			results <- source
			errs <- err
		})
	}
	wg.Wait()
	a, b := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if a == "" || a != b {
		t.Fatal("concurrent preparation generated two identities")
	}
}

func TestPrepareAccountOrdinaryStateWins(t *testing.T) {
	root := t.TempDir()
	_, err := prepareAccount(root, preparationOwner(), "one@example.test", preparationLimits(), func(source, target string) error {
		st, e := state.New(target)
		if e != nil {
			return e
		}
		if e = st.Close(); e != nil {
			return e
		}
		return publishPreparedAccount(source, target)
	})
	if !errors.Is(err, ErrPreparation) {
		t.Fatal("publication replaced competing state", err)
	}
	dir := filepath.Join(root, "users", preparationOwner().Mailbox)
	st, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.BindMailSource("imap"); err != nil {
		t.Fatal("competing state overwritten", err)
	}
	if _, err = os.Stat(filepath.Join(dir, preparationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest published over ordinary state")
	}
	// Even an empty directory created after the preflight is never replaced.
	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err = publishPreparedAccount(source, target); err == nil {
		t.Fatal("empty destination replaced")
	}
}

func TestPrepareAccountCrashHelper(t *testing.T) {
	root := os.Getenv("KYPOST_PREPARE_CRASH_ROOT")
	if root == "" {
		return
	}
	mode := os.Getenv("KYPOST_PREPARE_CRASH_MODE")
	_, err := prepareAccount(root, preparationOwner(), "one@example.test", preparationLimits(), func(source, target string) error {
		if mode == "after" {
			if e := publishPreparedAccount(source, target); e != nil {
				return e
			}
		}
		process, e := os.FindProcess(os.Getpid())
		if e != nil {
			return e
		}
		return process.Kill()
	})
	t.Fatalf("child not killed: %v", err)
}

func TestPrepareAccountKilledBeforeAndAfterPublication(t *testing.T) {
	for _, mode := range []string{"before", "after"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestPrepareAccountCrashHelper$")
			cmd.Env = append(os.Environ(), "KYPOST_PREPARE_CRASH_ROOT="+root, "KYPOST_PREPARE_CRASH_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var killed *exec.ExitError
			if !errors.As(err, &killed) {
				t.Fatalf("child not killed: %v %s", err, output)
			}
			status, ok := killed.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child failed before crash boundary: %v %s", err, output)
			}
			dir := filepath.Join(root, "users", preparationOwner().Mailbox)
			var before []byte
			if mode == "before" {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial directory published")
				}
			} else {
				var err error
				before, err = os.ReadFile(filepath.Join(dir, preparationFile))
				if err != nil {
					t.Fatal("published manifest missing", err)
				}
			}
			source, err := PrepareAccount(root, preparationOwner(), "one@example.test", preparationLimits())
			if err != nil || source == "" {
				t.Fatal("crash retry failed", err)
			}
			if mode == "after" {
				after, err := os.ReadFile(filepath.Join(dir, preparationFile))
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("lost ack changed namespace")
				}
			}
		})
	}
}
