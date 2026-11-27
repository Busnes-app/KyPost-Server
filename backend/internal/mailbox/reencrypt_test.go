package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestNativeArchiveReencryptCAS(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)

	raw1 := []byte("From: bob@example.test\r\nTo: alice@example.test\r\nSubject: One\r\n\r\nbody-v4-one\r\n")
	raw2 := []byte("From: alice@example.test\r\nTo: bob@example.test\r\nSubject: Two\r\n\r\nbody-v4-two\r\n")
	id1, err := s.Append(ctx, "INBOX", bytes.NewReader(raw1), false)
	must(t, err)
	id2, err := s.Append(ctx, "Sent", bytes.NewReader(raw2), false)
	must(t, err)

	// Set seen + starred + label on id1 to verify all flags survive CAS replacement.
	must(t, s.Update(ctx, "INBOX", id1, true, true, []string{"important"}))

	folders, err := c.ListAllFolders(ctx)
	must(t, err)
	if len(folders) < 2 {
		t.Fatalf("expected folders to include INBOX and Sent, got %v", folders)
	}

	uids, err := c.ListFolderMessageUIDs(ctx, "INBOX", 0, 10)
	must(t, err)
	if len(uids) != 1 || uids[0] != int(id1) {
		t.Fatalf("INBOX uids = %v, want [%d]", uids, id1)
	}
	sentUIDs, err := c.ListFolderMessageUIDs(ctx, "Sent", 0, 10)
	must(t, err)
	if len(sentUIDs) != 1 || sentUIDs[0] != int(id2) {
		t.Fatalf("Sent uids = %v, want [%d]", sentUIDs, id2)
	}

	gotRaw, err := c.GetRawMessage(ctx, "INBOX", int(id1))
	must(t, err)
	if !bytes.Equal(gotRaw, raw1) {
		t.Fatalf("GetRawMessage mismatch")
	}

	replacement := []byte("From: bob@example.test\r\nTo: alice@example.test\r\nSubject: One\r\n\r\nbody-v6-reencrypted-longer-payload\r\n")

	// Wrong CAS hash must fail with ErrCASMismatch and leave stored bytes untouched.
	if _, err := c.ReplaceRawMessageCAS(ctx, "INBOX", int(id1), sha256Hex([]byte("wrong")), replacement); !errors.Is(err, imapadapter.ErrCASMismatch) {
		t.Fatalf("expected ErrCASMismatch, got %v", err)
	}
	stillOld, err := c.GetRawMessage(ctx, "INBOX", int(id1))
	must(t, err)
	if !bytes.Equal(stillOld, raw1) {
		t.Fatalf("stored message mutated after failed CAS")
	}

	// Correct CAS hash replaces in-place preserving ID, flags, labels, and usage.
	newUID, err := c.ReplaceRawMessageCAS(ctx, "INBOX", int(id1), sha256Hex(raw1), replacement)
	must(t, err)
	if newUID != int(id1) {
		t.Fatalf("native CAS changed message ID: got %d want %d", newUID, id1)
	}

	gotNew, err := c.GetRawMessage(ctx, "INBOX", int(id1))
	must(t, err)
	if !bytes.Equal(gotNew, replacement) {
		t.Fatalf("stored raw after CAS mismatch")
	}

	// Replaying with the old hash must fail with ErrCASMismatch.
	if _, err := c.ReplaceRawMessageCAS(ctx, "INBOX", int(id1), sha256Hex(raw1), replacement); !errors.Is(err, imapadapter.ErrCASMismatch) {
		t.Fatalf("stale replay expected ErrCASMismatch, got %v", err)
	}

	// Flags, labels, folder, and usage counters must be preserved/updated accurately.
	msgs, err := s.List(ctx, "INBOX", 0, 10)
	must(t, err)
	if len(msgs) != 1 || msgs[0].ID != id1 || !msgs[0].Seen || !msgs[0].Starred || len(msgs[0].Labels) != 1 || msgs[0].Labels[0] != "important" {
		t.Fatalf("metadata after CAS corrupted: %+v", msgs)
	}

	var used int64
	must(t, s.db.QueryRow("SELECT payload_bytes FROM usage WHERE id=1").Scan(&used))
	if want := int64(len(replacement) + len(raw2)); used != want {
		t.Fatalf("usage.payload_bytes = %d, want %d", used, want)
	}
}
