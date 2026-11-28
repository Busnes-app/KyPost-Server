package mailbox

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
)

const sealMarker = "merger-terms-91c4"

func sealTestRaw(n int) []byte {
	return []byte("From: sender@outside.test\r\nTo: alice@example.test\r\nSubject: " + sealMarker + "\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\n" + strings.Repeat(sealMarker+" body line\r\n", n))
}

func installTestSeal(t *testing.T) {
	t.Helper()
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.test")
	must(t, err)
	SetSeal(func(_ context.Context, _ Owner, raw []byte) ([]byte, error) {
		return pgpmail.EncryptStoredMIME(raw, id.ArmoredPublicKey)
	})
	t.Cleanup(func() { SetSeal(nil) })
}

// assertSealed checks the DB rows (listed subject, stored bytes, read through any object
// body) and, on SQLite, every file under dir for the marker.
func assertSealed(t *testing.T, s *Store, dir, folder string, id int64) {
	t.Helper()
	ctx := context.Background()
	raw, err := s.Raw(ctx, folder, id)
	must(t, err)
	parsed, err := imapadapter.ParseRawContent(raw)
	if err != nil || !parsed.Content.PGPEncrypted || bytes.Contains(raw, []byte(sealMarker)) {
		t.Fatalf("message %d is not sealed: %v", id, err)
	}
	var subject string
	var inRow []byte
	must(t, s.db.QueryRow("SELECT subject,raw FROM messages WHERE id=?", id).Scan(&subject, &inRow))
	if strings.Contains(subject, sealMarker) || bytes.Contains(inRow, []byte(sealMarker)) {
		t.Fatal("plaintext in the messages row")
	}
	must(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(sealMarker)) {
			t.Errorf("plaintext in %s", path)
		}
		return err
	}))
}

// File imports (mbox, EML, IMAP) are sealed like delivered mail; re-importing the same
// message is still a duplicate; drafts are the user's own and are not sealed here.
func TestSealImportedMail(t *testing.T) {
	installTestSeal(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	raw := sealTestRaw(100)
	must(t, c.ImportMessage(ctx, "Archive", raw, ImportMeta{}))
	if err := c.ImportMessage(ctx, "Archive", raw, ImportMeta{}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("re-import of a sealed message: %v", err)
	}
	list, err := s.List(ctx, "Archive", 0, 10)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("one imported message expected: %+v", list)
	}
	assertSealed(t, s, dir, "Archive", list[0].ID)
	draft, err := s.Append(ctx, "Drafts", bytes.NewReader(testRaw), true)
	must(t, err)
	if got, err := s.Raw(ctx, "Drafts", draft); err != nil || !bytes.Equal(got, testRaw) {
		t.Fatal("a draft was sealed")
	}
}

// Mail whose sealed form exceeds the message limit is refused, never stored in plaintext.
func TestSealTooLargeRefuses(t *testing.T) {
	installTestSeal(t)
	ctx := context.Background()
	limits := Limits{MessageBytes: 64 << 10, PayloadBytes: 4 << 20, Records: 100}
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, limits)
	raw := sealTestRaw(2000) // ~58 KiB of plaintext: fits, sealed (armor) does not
	if int64(len(raw)) > limits.MessageBytes {
		t.Fatal("fixture too large")
	}
	if _, err := s.Import(ctx, testReceipt("big"), bytes.NewReader(raw)); !errors.Is(err, ErrSealTooLarge) || !errors.Is(err, ErrUnimportable) {
		t.Fatalf("oversized sealed mail: %v", err)
	}
	if list, err := s.List(ctx, "INBOX", 0, 10); err != nil || len(list) != 0 {
		t.Fatalf("stored anyway: %+v %v", list, err)
	}
}

// An owner without a usable key gets the mail as received (the seal returns it), and
// the backlog sweep's CAS replacement later seals it in place, subject column included.
func TestSealKeylessThenBacklog(t *testing.T) {
	SetSeal(func(_ context.Context, _ Owner, raw []byte) ([]byte, error) { return raw, nil })
	t.Cleanup(func() { SetSeal(nil) })
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	raw := sealTestRaw(100)
	msg, err := s.Import(ctx, testReceipt("keyless"), bytes.NewReader(raw))
	must(t, err)
	if got, err := s.Raw(ctx, "INBOX", msg); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("keyless mail not kept as received")
	}
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	refs, err := c.MessagesAfter(ctx, 0, 10)
	must(t, err)
	if len(refs) != 1 || refs[0] != (Ref{Folder: "INBOX", ID: msg}) {
		t.Fatalf("backlog listing: %+v", refs)
	}
	id, err := pgpmailtest.GenerateIdentity("Alice", "alice@example.test")
	must(t, err)
	sealed, err := pgpmail.EncryptStoredMIME(raw, id.ArmoredPublicKey)
	must(t, err)
	_, err = c.ReplaceRawMessageCAS(ctx, "INBOX", int(msg), hashIncoming(raw), sealed)
	must(t, err)
	c.Scrub(ctx)
	assertSealed(t, s, dir, "INBOX", msg)
}

// Large bodies go to object storage already sealed: the object read back is PGP/MIME.
func TestSealObjectBodies(t *testing.T) {
	f := objectBodiesForTest(t)
	installTestSeal(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	msg, err := s.Import(ctx, testReceipt("big"), bytes.NewReader(sealTestRaw(ObjectThreshold/len(sealMarker))))
	must(t, err)
	keys := f.keys(t, testOwner)
	if len(keys) != 1 {
		t.Fatalf("one object expected: %v", keys)
	}
	if bytes.Contains(f.raw(t, keys[0]), []byte(sealMarker)) {
		t.Fatal("plaintext in the object")
	}
	assertSealed(t, s, dir, "INBOX", msg)
}
