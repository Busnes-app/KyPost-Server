package ingress

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

var mailboxLimits = mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 20}
var importRaw = []byte("From: sender@outside.test\r\nTo: alice@example.test\r\nSubject: shared receipt\r\n\r\nraw body\x00\r\n")

func openMailbox(t *testing.T, root string, owner mailbox.Owner) *mailbox.Store {
	t.Helper()
	s, err := mailbox.Open(filepath.Join(root, owner.Mailbox), owner, mailboxLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func createImport(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	for _, entry := range []struct{ address, owner string }{{"alice@example.test", "alice"}, {"alias@example.test", "alice"}, {"hidden@example.test", "bob"}} {
		if err := s.SetRoute(ctx, proofRoute(entry.address, entry.owner, 1)); err != nil {
			t.Fatal(err)
		}
		if err := s.Bind(ctx, "maddy", id, "sender@outside.test", entry.address); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Accept(ctx, "maddy", id, "sender@outside.test", bytes.NewReader(importRaw)); err != nil {
		t.Fatal(err)
	}
}
func TestMailboxImportPartialFailureAndReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	createImport(t, s, "delivery")
	stores := map[string]*mailbox.Store{}
	resolve := func(owner mailbox.Owner) (*mailbox.Store, error) {
		if owner.Subject == "bob" {
			return nil, errors.New("mailbox unavailable")
		}
		store := openMailbox(t, root, owner)
		stores[owner.Subject] = store
		return store, nil
	}
	if err = s.Import(ctx, "maddy", "delivery", resolve); err == nil {
		t.Fatal("partial import acknowledged")
	}
	d, err := s.Get(ctx, "maddy", "delivery")
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, importRaw) {
		t.Fatalf("holding lost after partial failure: %+v %v", d, err)
	}
	list, err := stores["alice"].List(ctx, "INBOX", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("first mailbox not committed: %+v %v", list, err)
	}
	aliceID := list[0].ID
	// A user deletion between crash and retry must not recreate their message.
	if err = stores["alice"].Delete(ctx, "INBOX", aliceID); err != nil {
		t.Fatal(err)
	}
	for _, store := range stores {
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	// Model the lease deadline passing without sleeping five minutes.
	if _, err = s.db.Exec("UPDATE deliveries SET lease_until=0"); err != nil {
		t.Fatal(err)
	}
	resolve = func(owner mailbox.Owner) (*mailbox.Store, error) {
		store := openMailbox(t, root, owner)
		stores[owner.Subject] = store
		return store, nil
	}
	if err = s.Import(ctx, "maddy", "delivery", resolve); err != nil {
		t.Fatal(err)
	}
	d, err = s.Get(ctx, "maddy", "delivery")
	if err != nil || d.State != "imported" || len(d.Raw) != 0 {
		t.Fatalf("complete commit not acknowledged: %+v %v", d, err)
	}
	for owner, store := range stores {
		list, err = store.List(ctx, "INBOX", 0, 10)
		want := 1
		if owner == "alice" {
			want = 0
		}
		if err != nil || len(list) != want {
			t.Fatalf("owner %s duplicates/resurrection: %+v %v", owner, list, err)
		}
		if want == 1 {
			raw, err := store.Raw(ctx, "INBOX", list[0].ID)
			if err != nil || !bytes.Equal(raw, importRaw) {
				t.Fatal("recipient raw changed")
			}
		}
	}
}

func TestMailboxImporterRejectsWrongStoreAndReassignment(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createImport(t, s, "wrong")
	wrong := openMailbox(t, root, mailbox.Owner{Issuer: "https://identity.example.test", Subject: "mallory", Mailbox: "mallory"})
	if err = s.Import(ctx, "maddy", "wrong", func(mailbox.Owner) (*mailbox.Store, error) { return wrong, nil }); !errors.Is(err, mailbox.ErrOwner) {
		t.Fatalf("wrong store: %v", err)
	}
	list, err := wrong.List(ctx, "INBOX", 0, 10)
	if err != nil || len(list) != 0 {
		t.Fatal("wrong owner received mail")
	}
	createImport(t, s, "reassign")
	if err = s.SetRoute(ctx, proofRoute("alice@example.test", "mallory", 2)); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = s.Import(ctx, "maddy", "reassign", func(mailbox.Owner) (*mailbox.Store, error) { called = true; return wrong, nil }); !errors.Is(err, ErrRoute) || called {
		t.Fatalf("reassigned owner resolved: %v %v", err, called)
	}
	d, err := s.Get(ctx, "maddy", "reassign")
	if err != nil || d.State != "quarantined" || !bytes.Equal(d.Raw, importRaw) {
		t.Fatal("reassignment lost holding bytes")
	}
}

func TestMailboxImporterCrashHelper(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		return
	}
	root, ready := args[1], args[2]
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Import(context.Background(), "maddy", "crash", func(owner mailbox.Owner) (*mailbox.Store, error) {
		if owner.Subject == "bob" {
			if err := os.WriteFile(ready, []byte("first committed"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {}
		}
		return mailbox.Open(filepath.Join(root, owner.Mailbox), owner, mailboxLimits)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestMailboxImporterSurvivesKilledWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	createImport(t, s, "crash")
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMailboxImporterCrashHelper$", "--", root, ready)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("helper did not commit: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	s, err = Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.Get(ctx, "maddy", "crash")
	if err != nil || !bytes.Equal(d.Raw, importRaw) {
		t.Fatal("writer death lost accepted mail")
	}
	if _, err = s.db.Exec("UPDATE deliveries SET lease_until=0"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*mailbox.Store{}
	if err = s.Import(ctx, "maddy", "crash", func(owner mailbox.Owner) (*mailbox.Store, error) {
		store := openMailbox(t, root, owner)
		stores[owner.Subject] = store
		return store, nil
	}); err != nil {
		t.Fatal(err)
	}
	for owner, store := range stores {
		list, err := store.List(ctx, "INBOX", 0, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("crash recovery %s: %+v %v", owner, list, err)
		}
	}
	d, err = s.Get(ctx, "maddy", "crash")
	if err != nil || d.State != "imported" || len(d.Raw) != 0 {
		t.Fatal("all mailbox commits not acknowledged")
	}
}

func TestMailboxImporterRetainsBytesOnMidImportReassignment(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createImport(t, s, "changing")
	stores := map[string]*mailbox.Store{}
	err = s.Import(ctx, "maddy", "changing", func(owner mailbox.Owner) (*mailbox.Store, error) {
		if owner.Subject == "bob" {
			if err := s.SetRoute(ctx, proofRoute("alice@example.test", "mallory", 2)); err != nil {
				return nil, err
			}
		}
		store := openMailbox(t, root, owner)
		stores[owner.Subject] = store
		return store, nil
	})
	if !errors.Is(err, ErrRoute) {
		t.Fatalf("reassignment acknowledged: %v", err)
	}
	d, err := s.Get(ctx, "maddy", "changing")
	if err != nil || d.State != "quarantined" || !bytes.Equal(d.Raw, importRaw) {
		t.Fatal("mid-import route change lost holding bytes")
	}
	// Committed copies remain with frozen owners; no address is resolved anew.
	for owner, store := range stores {
		if owner == "mallory" {
			t.Fatal("new owner received old mail")
		}
		list, err := store.List(ctx, "INBOX", 0, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("frozen copy lost for %s: %v", owner, err)
		}
	}
}
