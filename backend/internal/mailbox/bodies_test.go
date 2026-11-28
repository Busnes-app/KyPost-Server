package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/s3"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/vol/objecttest"
)

const bodyOrg = "0d6c8d5e-5f4e-4c1a-9a43-6f2b1c0e7a01"

// largeRaw is a message over ObjectThreshold carrying a marker the bucket must never hold.
func largeRaw(marker string) []byte {
	return fmt.Appendf(nil, "From: sender@outside.test\r\nTo: alice@example.test\r\nSubject: %s\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\n%s", marker, strings.Repeat(marker+" attachment\r\n", ObjectThreshold/len(marker)))
}

type bodyFixture struct {
	o     *vol.Object
	c     *s3.Client
	cell  string
	scope func(Owner) BodyScope
}

// objectBodiesForTest installs object storage for every store the test opens; scope places
// each owner (default: one org in the fixture's cell).
func objectBodiesForTest(t *testing.T) *bodyFixture {
	t.Helper()
	f := &bodyFixture{cell: objecttest.Cell()}
	f.o, f.c = objecttest.New(t, f.cell)
	f.scope = func(Owner) BodyScope { return BodyScope{Cell: f.cell, Org: bodyOrg} }
	objectBodies = func(o Owner) (*vol.Object, BodyScope, error) { return f.o, f.scope(o), nil }
	t.Cleanup(func() { objectBodies = nil })
	return f
}

func (f *bodyFixture) keys(t *testing.T, owner Owner) []string {
	t.Helper()
	sc := f.scope(owner)
	keys, err := f.o.List(context.Background(), vol.BodyPrefix(sc.Cell, sc.Org, owner.Mailbox))
	must(t, err)
	return keys
}

func (f *bodyFixture) raw(t *testing.T, key string) []byte {
	t.Helper()
	rc, _, err := f.c.Get(context.Background(), key, "")
	must(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	must(t, err)
	return b
}

func usage(t *testing.T, s *Store) (records, bytes int64) {
	t.Helper()
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage WHERE id=1").Scan(&records, &bytes))
	return
}

// A large body goes to the bucket sealed and reads back; a small one stays in raw; usage
// and listed sizes equal a store without object storage.
func TestObjectBodies(t *testing.T) {
	ctx := context.Background()
	big, small := largeRaw("quarterly-plan-7f3a"), testRaw
	plain := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	for i, raw := range [][]byte{big, small} {
		_, err := plain.Import(ctx, testReceipt(fmt.Sprint(i)), bytes.NewReader(raw))
		must(t, err)
	}
	f := objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	bigID, err := s.Import(ctx, testReceipt("0"), bytes.NewReader(big))
	must(t, err)
	smallID, err := s.Append(ctx, "INBOX", bytes.NewReader(small), false)
	must(t, err)

	var stored int64
	must(t, s.db.QueryRow("SELECT length(raw) FROM messages WHERE id=?", bigID).Scan(&stored))
	if stored != 0 {
		t.Fatalf("large body left %d bytes in raw", stored)
	}
	must(t, s.db.QueryRow("SELECT length(raw) FROM messages WHERE id=?", smallID).Scan(&stored))
	if stored != int64(len(small)) {
		t.Fatal("small body moved")
	}
	keys := f.keys(t, testOwner)
	if len(keys) != 1 {
		t.Fatalf("bucket holds %d objects", len(keys))
	}
	if obj := f.raw(t, keys[0]); bytes.Contains(obj, []byte("quarterly-plan-7f3a")) || bytes.Contains(obj, []byte("attachment")) {
		t.Fatal("the bucket holds plaintext")
	}
	for id, want := range map[int64][]byte{bigID: big, smallID: small} {
		got, err := s.Raw(ctx, "INBOX", id)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("message %d read back: %v", id, err)
		}
	}
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	if got, err := c.GetRawMessage(ctx, "INBOX", int(bigID)); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("client read: %v", err)
	}
	r1, b1 := usage(t, s)
	r2, b2 := usage(t, plain)
	if r1 != r2 || b1 != b2 {
		t.Fatalf("usage %d/%d, without object storage %d/%d", r1, b1, r2, b2)
	}
	list, err := s.List(ctx, "INBOX", 0, 10)
	must(t, err)
	for _, m := range list {
		if m.ID == bigID && m.Size != int64(len(big)) {
			t.Fatalf("listed size %d", m.Size)
		}
	}
	var recount int64
	must(t, s.db.QueryRow("SELECT (SELECT coalesce(sum(length(raw)),0) FROM messages)+(SELECT coalesce(sum(size),0) FROM bodies)").Scan(&recount))
	if recount != b1 {
		t.Fatalf("recount %d, usage %d", recount, b1)
	}
}

// A crash between the object write and the commit acknowledges nothing and leaves one
// orphan, which the sweep keeps for the retention and then deletes; the redelivery is stored.
func TestObjectBodyCrashBeforeCommit(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	big := largeRaw("crash-body")
	crash := errors.New("crash")
	bodyCommitHook = func() error { return crash }
	_, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	bodyCommitHook = nil
	if !errors.Is(err, crash) {
		t.Fatal(err)
	}
	var n int
	must(t, s.db.QueryRow("SELECT count(*) FROM messages").Scan(&n))
	if n != 0 {
		t.Fatal("a row committed")
	}
	if len(f.keys(t, testOwner)) != 1 {
		t.Fatal("the crash should leave the uploaded object")
	}
	if d, err := s.SweepBodies(ctx); err != nil || d != 0 {
		t.Fatalf("first sweep deleted %d: %v", d, err)
	}
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	must(t, err)
	if got, err := s.Raw(ctx, "INBOX", id); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("redelivery lost: %v", err)
	}
	// The retention passes for the orphan; the named object survives every sweep.
	_, err = s.db.Exec("UPDATE body_orphans SET seen_at=seen_at-?", int64(BodyRetention.Seconds()))
	must(t, err)
	if d, err := s.SweepBodies(ctx); err != nil || d != 1 {
		t.Fatalf("sweep after retention deleted %d: %v", d, err)
	}
	if len(f.keys(t, testOwner)) != 1 {
		t.Fatal("sweep left the orphan or took the named body")
	}
	if got, err := s.Raw(ctx, "INBOX", id); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("named body after sweep: %v", err)
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM body_orphans").Scan(&n))
	if n != 0 {
		t.Fatal("orphan record kept")
	}
}

// If the object cannot be written, no row commits: a row never names a missing body.
func TestObjectBodyWriteFailureCommitsNothing(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	t.Setenv("KY_TEST_S3_SECRET_KEY", "wrong-secret")
	f.o, _ = objecttest.New(t, f.cell)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	if _, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(largeRaw("lost"))); err == nil {
		t.Fatal("stored without its object")
	}
	var n int
	must(t, s.db.QueryRow("SELECT count(*) FROM messages").Scan(&n))
	if r, b := usage(t, s); n != 0 || r != 0 || b != 0 {
		t.Fatal("a row or usage committed")
	}
}

// A refused or replayed write deletes the object it uploaded.
func TestObjectBodyRefusalReleasesObject(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	big := largeRaw("replay")
	_, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	must(t, err)
	_, err = s.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	must(t, err)
	if _, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(largeRaw("other"))); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if n := len(f.keys(t, testOwner)); n != 1 {
		t.Fatalf("%d objects for one message", n)
	}
}

// An expunge tombstones the object and stops counting it; the sweep deletes it only after
// the retention, so a capsule taken before the expunge restores the body.
func TestObjectBodyExpungeLateDelete(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	big := largeRaw("expunged")
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	must(t, err)
	_, before := usage(t, s)
	must(t, s.Delete(ctx, "INBOX", id))
	if _, after := usage(t, s); before-after != int64(len(big)) {
		t.Fatalf("expunge released %d bytes", before-after)
	}
	var n int
	must(t, s.db.QueryRow("SELECT count(*) FROM body_tombstones").Scan(&n))
	if n != 1 {
		t.Fatal("no tombstone")
	}
	if d, err := s.SweepBodies(ctx); err != nil || d != 0 || len(f.keys(t, testOwner)) != 1 {
		t.Fatalf("early sweep deleted %d: %v", d, err)
	}
	_, err = s.db.Exec("UPDATE body_tombstones SET at=at-?", int64(BodyRetention.Seconds()))
	must(t, err)
	if d, err := s.SweepBodies(ctx); err != nil || d != 1 || len(f.keys(t, testOwner)) != 0 {
		t.Fatalf("late sweep deleted %d: %v", d, err)
	}
}

// One org's store never reads another's object: a forged row resolves under its own prefix,
// and ciphertext copied there fails to open. Neither returns empty mail.
func TestObjectBodyCrossOrgRefused(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	orgB := "0d6c8d5e-5f4e-4c1a-9a43-6f2b1c0e7a02"
	f.scope = func(o Owner) BodyScope {
		if o.Mailbox == "bob" {
			return BodyScope{Cell: f.cell, Org: orgB}
		}
		return BodyScope{Cell: f.cell, Org: bodyOrg}
	}
	alice := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	bobOwner := Owner{Issuer: testOwner.Issuer, Subject: "bob", Mailbox: "bob"}
	bob := openTest(t, filepath.Join(t.TempDir(), "mailbox"), bobOwner, testLimits)
	big := largeRaw("alice-only")
	_, err := alice.Import(ctx, testReceipt("one"), bytes.NewReader(big))
	must(t, err)
	var object string
	var size int64
	var sum []byte
	must(t, alice.db.QueryRow("SELECT object,size,sha256 FROM bodies").Scan(&object, &size, &sum))
	bobID, err := bob.Append(ctx, "INBOX", bytes.NewReader(testRaw), false)
	must(t, err)
	_, err = bob.db.Exec("UPDATE messages SET raw=? WHERE id=?", []byte{}, bobID)
	must(t, err)
	_, err = bob.db.Exec("INSERT INTO bodies VALUES(?,?,?,?)", bobID, object, size, sum)
	must(t, err)
	if got, err := bob.Raw(ctx, "INBOX", bobID); err == nil || got != nil {
		t.Fatalf("bob read alice's object reference: %v", err)
	}
	sealed := f.raw(t, vol.BodyPrefix(f.cell, bodyOrg, "alice")+object)
	_, err = f.c.Put(ctx, vol.BodyPrefix(f.cell, orgB, "bob")+object, bytes.NewReader(sealed), int64(len(sealed)))
	must(t, err)
	if got, err := bob.Raw(ctx, "INBOX", bobID); err == nil || got != nil {
		t.Fatalf("bob opened alice's copied ciphertext: %v", err)
	}
}

// A mailbox placed in a cell this deployment does not serve stores nothing.
func TestObjectBodyOtherCellRefused(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	f.scope = func(Owner) BodyScope { return BodyScope{Cell: objecttest.Cell(), Org: bodyOrg} }
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	if _, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(largeRaw("foreign"))); !errors.Is(err, vol.ErrCell) {
		t.Fatal(err)
	}
	var n int
	must(t, s.db.QueryRow("SELECT count(*) FROM messages").Scan(&n))
	if n != 0 {
		t.Fatal("stored in another cell")
	}
}

// Re-encrypting a moved body writes a new object and tombstones the old one.
func TestObjectBodyReencrypt(t *testing.T) {
	ctx := context.Background()
	f := objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	old, next := largeRaw("before"), largeRaw("after-reencryption")
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(old))
	must(t, err)
	var oldObject string
	must(t, s.db.QueryRow("SELECT object FROM bodies").Scan(&oldObject))
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	sum := sha256.Sum256(old)
	_, err = c.ReplaceRawMessageCAS(ctx, "INBOX", int(id), hex.EncodeToString(sum[:]), next)
	must(t, err)
	if got, err := s.Raw(ctx, "INBOX", id); err != nil || !bytes.Equal(got, next) {
		t.Fatalf("re-encrypted body: %v", err)
	}
	var tomb string
	must(t, s.db.QueryRow("SELECT object FROM body_tombstones").Scan(&tomb))
	if tomb != oldObject || len(f.keys(t, testOwner)) != 2 {
		t.Fatal("old object not tombstoned beside the new one")
	}
	if _, b := usage(t, s); b != int64(len(next)) {
		t.Fatalf("usage %d after re-encryption", b)
	}
}

// A bodies row whose size or digest does not match its object fails the read.
func TestObjectBodyRowMismatchRefused(t *testing.T) {
	ctx := context.Background()
	objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(largeRaw("digest")))
	must(t, err)
	for forge, args := range map[string][]any{"UPDATE bodies SET sha256=?": {make([]byte, 32)}, "UPDATE bodies SET size=size-1": nil} {
		tx, err := s.db.Begin()
		must(t, err)
		_, err = tx.Exec(forge, args...)
		must(t, err)
		_, err = s.body(ctx, tx, id, []byte{}, s.limits.MessageBytes)
		must(t, tx.Rollback())
		if !errors.Is(err, errBody) {
			t.Errorf("%s: %v", forge, err)
		}
	}
}

// A body expunged between reading raw and its bodies row is reported gone, never empty.
func TestObjectBodyVanishedBetweenReadsIsNotFound(t *testing.T) {
	ctx := context.Background()
	objectBodiesForTest(t)
	s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, testLimits)
	id, err := s.Import(ctx, testReceipt("one"), bytes.NewReader(largeRaw("vanishing")))
	must(t, err)
	_, err = s.db.Exec("DELETE FROM bodies WHERE message_id=?", id) // what a concurrent expunge leaves
	must(t, err)
	if got, err := s.Raw(ctx, "INBOX", id); !errors.Is(err, ErrNotFound) || got != nil {
		t.Fatalf("read after the body vanished: %q %v", got, err)
	}
}
