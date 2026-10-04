package mailbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeReferenceGenerationRollbackAndFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	first, err := s.Import(ctx, testReceipt("first"), bytes.NewReader(testRaw))
	must(t, err)
	source := (&Client{store: s}).MailSourceIdentity()
	before := s.MessageReferenceGeneration()
	snapshotDir := filepath.Join(t.TempDir(), "mailbox")
	must(t, os.Mkdir(snapshotDir, 0700))
	snapshot := filepath.Join(snapshotDir, "mailbox.db")
	_, err = s.db.Exec("VACUUM INTO ?", snapshot)
	must(t, err)
	later, err := s.Import(ctx, testReceipt("later"), bytes.NewReader(testRaw))
	must(t, err)
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	if s.MessageReferenceGeneration() != before {
		t.Fatal("ordinary reopen rotated generation")
	}
	// A real earlier SQLite snapshot rewinds AUTOINCREMENT. Rotation must not
	// change its cryptographic namespace or its committed mail/receipt identity.
	must(t, RotateRestoredMessageReferences(snapshot, source))
	restored, err := OpenExisting(filepath.Dir(snapshot), testOwner, testLimits, source)
	must(t, err)
	defer restored.Close()
	if restored.MessageReferenceGeneration() == before {
		t.Fatal("restored generation reused")
	}
	if (&Client{store: restored}).MailSourceIdentity() != source {
		t.Fatal("encryption source changed")
	}
	got, err := restored.Raw(ctx, "INBOX", first)
	must(t, err)
	if !bytes.Equal(got, testRaw) {
		t.Fatal("committed raw changed")
	}
	replay, err := restored.Import(ctx, testReceipt("first"), bytes.NewReader(testRaw))
	must(t, err)
	if replay != first {
		t.Fatal("receipt changed")
	}
	reused, err := restored.Import(ctx, testReceipt("after-rollback"), bytes.NewReader(testRaw))
	must(t, err)
	if reused != later {
		t.Fatal("fixture did not reproduce ID reuse")
	}
	must(t, restored.Close())
	old := s.MessageReferenceGeneration()
	path := filepath.Join(dir, "mailbox.db")
	if err := RotateRestoredMessageReferences(path, source+"wrong"); !errors.Is(err, ErrPreparation) {
		t.Fatal("wrong source accepted", err)
	}
	_, err = s.db.Exec("CREATE TRIGGER refuse_generation BEFORE UPDATE ON reference_generation BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END")
	must(t, err)
	if err := RotateRestoredMessageReferences(path, source); err == nil {
		t.Fatal("failed write reported success")
	}
	current, err := messageReferenceGeneration(s.db)
	must(t, err)
	if current != old {
		t.Fatal("failed rotation changed generation")
	}
	if err := RotateRestoredMessageReferences(filepath.Join(t.TempDir(), "missing.db"), source); err == nil {
		t.Fatal("missing database recreated")
	}
}

func TestNativeReferenceGenerationOlderSchemaAndCorruption(t *testing.T) {
	for _, mode := range []string{"older", "missing-row", "invalid", "view"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mailbox")
			s := openTest(t, dir, testOwner, testLimits)
			source := (&Client{store: s}).MailSourceIdentity()
			switch mode {
			case "older":
				_, err := s.db.Exec("DROP TABLE reference_generation")
				must(t, err)
			case "missing-row":
				_, err := s.db.Exec("DELETE FROM reference_generation")
				must(t, err)
			case "invalid":
				_, err := s.db.Exec("UPDATE reference_generation SET token='bad'")
				must(t, err)
			case "view":
				_, err := s.db.Exec("DROP TABLE reference_generation; CREATE VIEW reference_generation AS SELECT 1 AS id,'bad' AS token")
				must(t, err)
			}
			generation, readErr := messageReferenceGeneration(s.db)
			if mode == "older" {
				if readErr != nil || generation != "" {
					t.Fatal("older metadata is not optional", readErr)
				}
			} else if readErr == nil {
				t.Fatal("corrupt metadata accepted read-only")
			}
			must(t, s.Close())
			rotateErr := RotateRestoredMessageReferences(filepath.Join(dir, "mailbox.db"), source)
			opened, openErr := OpenExisting(dir, testOwner, testLimits, source)
			if mode == "older" {
				must(t, rotateErr)
				must(t, openErr)
				defer opened.Close()
				if opened.MessageReferenceGeneration() == "" {
					t.Fatal("older restore did not gain generation")
				}
			} else {
				if rotateErr == nil || openErr == nil {
					if opened != nil {
						_ = opened.Close()
					}
					t.Fatal("corruption silently reset", rotateErr, openErr)
				}
			}
		})
	}
}
