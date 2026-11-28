package ingress

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

// sealMarker is in the subject and in every body line: after import it may be nowhere
// in plaintext, neither in a mailbox nor in the buffer.
const sealMarker = "merger-terms-91c4"

// Over a page, so the body spills into SQLite overflow pages that secure_delete must zero.
var sealRaw = []byte("From: sender@outside.test\r\nTo: alice@example.test\r\nSubject: " + sealMarker + "\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\n" + strings.Repeat(sealMarker+" body line\r\n", 2000))

// installSeal seals every incoming message to one test key, as processor.IncomingSeal
// does for an owner with a usable key.
func installSeal(t *testing.T) {
	t.Helper()
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	removeSeal := mailbox.InstallSeal(func(_ context.Context, _ mailbox.Owner, raw []byte) ([]byte, error) {
		return pgpmail.EncryptStoredMIME(raw, id.ArmoredPublicKey)
	})
	t.Cleanup(removeSeal)
}

func acceptSealed(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	for _, owner := range []string{"alice", "bob"} {
		address := owner + "@example.test"
		if err := s.SetRoute(ctx, proofRoute(address, owner, 1)); err != nil {
			t.Fatal(err)
		}
		if err := s.Bind(ctx, "maddy", id, "sender@outside.test", address); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Accept(ctx, "maddy", id, "sender@outside.test", bytes.NewReader(sealRaw)); err != nil {
		t.Fatal(err)
	}
}

// noPlaintext fails when the marker is in any file under dir: SQLite databases, WALs and
// journals. On Postgres the files are not there, and the row checks below carry it.
func noPlaintext(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(sealMarker)) {
			t.Errorf("plaintext in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sealedRows checks a mailbox's one INBOX message: its listed subject and stored bytes
// carry no plaintext and are PGP/MIME.
func sealedRows(t *testing.T, store *mailbox.Store) {
	t.Helper()
	ctx := context.Background()
	list, err := store.List(ctx, "INBOX", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("one message expected: %+v %v", list, err)
	}
	raw, err := store.Raw(ctx, "INBOX", list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := imapadapter.ParseRawContent(raw)
	if err != nil || !parsed.Content.PGPEncrypted {
		t.Fatalf("stored message is not PGP/MIME: %v", err)
	}
	if strings.Contains(list[0].Subject, sealMarker) || bytes.Contains(raw, []byte(sealMarker)) {
		t.Fatal("plaintext in the mailbox rows")
	}
}

func bufferGone(t *testing.T, s *Store, root, id string) {
	t.Helper()
	d, err := s.Get(context.Background(), "maddy", id)
	if err != nil || d.State != "archived" || len(d.Raw) != 0 {
		t.Fatalf("delivery not archived without bytes: %+v %v", d.State, err)
	}
	noPlaintext(t, root)
}

// Every way into a mailbox (native SMTP, relay and Cloudflare pickup through Import,
// quarantine Release, multi mode's ImportMailbox) stores ciphertext, and after the
// acknowledgement the buffer's bytes are gone from ingress.db and its WAL.
func TestImportSealsEveryPath(t *testing.T) {
	installSeal(t)
	ctx := context.Background()
	for _, path := range []string{"import", "release", "obligations"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			s, err := Open(filepath.Join(root, "holding"), proofLimits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			stores := map[string]*mailbox.Store{}
			resolve := func(owner mailbox.Owner) (*mailbox.Store, error) {
				if stores[owner.Subject] == nil {
					stores[owner.Subject] = openMailbox(t, root, owner)
				}
				return stores[owner.Subject], nil
			}
			acceptSealed(t, s, "d")
			switch path {
			case "import":
				err = s.Import(ctx, "maddy", "d", resolve)
			case "release":
				if err := s.SetRoute(ctx, proofRoute("alice@example.test", "mallory", 2)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Claim(ctx, "maddy", "d", time.Minute); !errors.Is(err, ErrRoute) {
					t.Fatal("not quarantined", err)
				}
				err = s.Release(ctx, "maddy", "d", resolve)
			case "obligations":
				d, gerr := s.Get(ctx, "maddy", "d")
				if gerr != nil {
					t.Fatal(gerr)
				}
				done := map[string]Done{}
				for _, m := range []string{"alice", "bob"} {
					store, _ := resolve(mailbox.Owner{Issuer: "https://identity.example.test", Subject: m, Mailbox: m})
					if err := s.ImportMailbox(ctx, d, "INBOX", store); err != nil {
						t.Fatal(err)
					}
					done[m] = Imported
				}
				_, err = s.Complete(ctx, "maddy", "d", done)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, store := range stores {
				sealedRows(t, store)
			}
			bufferGone(t, s, root, "d")
		})
	}
}

// A crash after sealing and before the mailbox commit acknowledges nothing: alice's
// copy is committed, bob's sealed copy dies before its commit, and the buffer keeps
// the delivery. The retry seals again; alice's receipt matches on the received bytes'
// digest despite the new ciphertext, so each mailbox holds exactly one sealed copy.
func TestSealCrashBeforeCommitLosesNothing(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "holding"), proofLimits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	acceptSealed(t, s, "d")
	stores := map[string]*mailbox.Store{}
	resolve := func(owner mailbox.Owner) (*mailbox.Store, error) {
		if stores[owner.Subject] == nil {
			stores[owner.Subject] = openMailbox(t, root, owner)
		}
		return stores[owner.Subject], nil
	}
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	// The process dies between bob's seal and his commit: the context ends there.
	crash, die := context.WithCancel(context.Background())
	removeSeal := mailbox.InstallSeal(func(_ context.Context, owner mailbox.Owner, raw []byte) ([]byte, error) {
		sealed, err := pgpmail.EncryptStoredMIME(raw, id.ArmoredPublicKey)
		if owner.Subject == "bob" {
			die()
		}
		return sealed, err
	})
	t.Cleanup(removeSeal)
	if err := s.Import(crash, "maddy", "d", resolve); err == nil {
		t.Fatal("import survived the crash")
	}
	d, err := s.Get(context.Background(), "maddy", "d")
	if err != nil || d.State != "pending" || !bytes.Equal(d.Raw, sealRaw) {
		t.Fatalf("buffer lost the delivery: %v %v", d.State, err)
	}
	if list, err := stores["bob"].List(context.Background(), "INBOX", 0, 10); err != nil || len(list) != 0 {
		t.Fatalf("half-stored message: %+v %v", list, err)
	}
	sealedRows(t, stores["alice"])
	// Model the lease deadline passing without sleeping five minutes.
	if _, err = s.db.Exec("UPDATE deliveries SET lease_until=0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "maddy", "d", resolve); err != nil {
		t.Fatal(err)
	}
	for _, store := range stores {
		sealedRows(t, store)
	}
	bufferGone(t, s, root, "d")
}
