package mailbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
)

type incomingFixture struct {
	Source     imapadapter.IncomingSource
	Marker     string
	Ciphertext []byte
}

func incomingForTest(t *testing.T, c *Client, uid int) (incomingFixture, *pgpmail.Identity) {
	t.Helper()
	source, err := c.PrepareIncoming(context.Background(), uid, true)
	must(t, err)
	key, err := pgpmail.GenerateIdentity("Alice", "alice@example.test")
	must(t, err)
	cipher, err := pgpmail.EncryptStoredMIME(source.Raw, key.ArmoredPublicKey)
	must(t, err)
	marker := strings.Repeat("ab", 32)
	return incomingFixture{Source: source, Marker: marker, Ciphertext: append([]byte("X-KyPost-Incoming: "+marker+"\r\n"), cipher...)}, key
}
func TestNativeIncomingReplacementAtomicRecovery(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	raw := mailmsg.Message{From: "sender@outside.test", To: []string{"alice@example.test"}, Subject: "private original subject", Body: "private original body", Attachments: []mailmsg.Attachment{{Name: "original.bin", Content: []byte("binary\x00exact")}}}.Build()
	uid := importClient(t, s, "encrypted", raw)
	f, key := incomingForTest(t, c, uid)
	// Flags changed after prepare must survive; never use the saved snapshot.
	must(t, s.Update(ctx, "INBOX", int64(uid), true, true, []string{"Travel", "$Phishing"}))
	_, before, err := s.Changes(ctx, 0, 100)
	must(t, err)
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	c, err = NewClient(s, "alice@example.test")
	must(t, err)
	replacement, err := c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext)
	must(t, err)
	if replacement <= uid {
		t.Fatal("replacement reused original ID")
	}
	if _, err = s.Raw(ctx, "INBOX", int64(uid)); !errors.Is(err, ErrNotFound) {
		t.Fatal("live plaintext retained")
	}
	copies, err := s.List(ctx, "INBOX", 0, 10)
	must(t, err)
	if len(copies) != 1 || !copies[0].Seen || !copies[0].Starred || len(copies[0].Labels) != 2 || copies[0].AtUTC != f.Source.Date.UTC().Format(time.RFC3339) {
		t.Fatalf("current metadata lost: %+v", copies)
	}
	if copies[0].Subject != pgpmail.OuterPlaceholderSubject {
		t.Fatal("original subject in replacement metadata")
	}
	changes, _, err := s.Changes(ctx, before, 100)
	must(t, err)
	if len(changes) != 2 || changes[0].ID != int64(uid) || !changes[0].Removed || changes[1].ID != int64(replacement) || changes[1].Removed {
		t.Fatalf("replacement deltas: %+v", changes)
	}
	bodies, err := c.GetMessageBodies(ctx, "INBOX", []int{replacement})
	must(t, err)
	decrypted, err := pgpmail.DecryptMIME(bodies[replacement].PGPEncryptedPayload, key, nil)
	must(t, err)
	if !bytes.Contains(decrypted.Content, raw) {
		t.Fatal("encrypted replacement lost original MIME")
	}
	// A second writer recovers exact bytes even after a committed move/stop.
	second := openTest(t, dir, testOwner, testLimits)
	other, err := NewClient(second, "alice@example.test")
	must(t, err)
	got, err := other.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext)
	must(t, err)
	if got != replacement {
		t.Fatal("replacement retry duplicated mail")
	}
	for _, action := range []string{"keyword", "read", "move", "stop"} {
		value := "Bills"
		if action == "move" {
			value = "INBOX/Filed"
		}
		must(t, c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, action, value))
	}
	must(t, s.Close())
	s = openTest(t, dir, testOwner, testLimits)
	c, err = NewClient(s, "alice@example.test")
	must(t, err)
	must(t, c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "move", "INBOX/Filed"))
	must(t, c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "stop", ""))
	got, err = c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext)
	must(t, err)
	if got != replacement {
		t.Fatal("reopen replaced twice")
	}
	if err = c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "keyword", "Bad"); err == nil {
		t.Fatal("action followed a moved copy")
	}
	replay, err := s.Import(ctx, testReceipt("encrypted"), bytes.NewReader(raw))
	must(t, err)
	if replay != int64(uid) {
		t.Fatal("transport receipt changed ownership/ID")
	}
	must(t, s.Delete(ctx, "INBOX/Filed", int64(replacement)))
	if _, err = c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext); err == nil {
		t.Fatal("deleted encrypted copy resurrected")
	}
}

func TestNativeIncomingFencesAndRollback(t *testing.T) {
	ctx := context.Background()
	s, c := newTestClient(t)
	uid := importClient(t, s, "one", testRaw)
	f, _ := incomingForTest(t, c, uid)
	recreated, other := newTestClient(t)
	importClient(t, recreated, "one", testRaw)
	if _, err := other.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext); err == nil {
		t.Fatal("same-owner recreated database accepted old source")
	}
	changed := f.Source
	changed.UIDValidity++
	if _, err := c.ReplaceIncoming(ctx, changed, f.Marker, f.Ciphertext); err == nil {
		t.Fatal("different namespace accepted")
	}
	if _, err := c.ReplaceIncoming(ctx, f.Source, "bad", f.Ciphertext); err == nil {
		t.Fatal("invalid marker accepted")
	}
	if _, err := c.ReplaceIncoming(ctx, f.Source, f.Marker, append([]byte("X-KyPost-Incoming: "+f.Marker+"\r\n"), testRaw...)); err == nil {
		t.Fatal("plaintext accepted as replacement")
	}
	must(t, s.CreateFolder(ctx, "INBOX/Other"))
	must(t, s.Move(ctx, "INBOX", int64(uid), "INBOX/Other"))
	if _, err := c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext); !errors.Is(err, ErrNotFound) {
		t.Fatal("moved original replaced")
	}
	must(t, s.Move(ctx, "INBOX/Other", int64(uid), "INBOX"))
	_, err := s.db.Exec("UPDATE messages SET raw=? WHERE id=?", append(bytes.Clone(testRaw), 'x'), uid)
	must(t, err)
	if _, err = c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext); err == nil {
		t.Fatal("changed original deleted")
	}
	_, err = s.db.Exec("UPDATE messages SET raw=? WHERE id=?", testRaw, uid)
	must(t, err)
	_, before, err := s.Changes(ctx, 0, 100)
	must(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON incoming_replacements BEGIN SELECT RAISE(ABORT,'test transaction rollback'); END`)
	must(t, err)
	if _, err = c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext); err == nil {
		t.Fatal("forced receipt failure ignored")
	}
	live, err := s.Raw(ctx, "INBOX", int64(uid))
	must(t, err)
	changes, _, err := s.Changes(ctx, before, 100)
	must(t, err)
	if !bytes.Equal(live, testRaw) || len(changes) != 0 {
		t.Fatal("rollback lost original or emitted partial changes")
	}
	var records, used int64
	must(t, s.db.QueryRow("SELECT records,payload_bytes FROM usage").Scan(&records, &used))
	if records != 1 || used != int64(len(testRaw)) {
		t.Fatal("rollback corrupted quota")
	}
	_, err = s.db.Exec("DROP TRIGGER reject_receipt")
	must(t, err)
	replacement, err := c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext)
	must(t, err)
	if _, err = c.ReplaceIncoming(ctx, f.Source, strings.Repeat("cd", 32), f.Ciphertext); err == nil {
		t.Fatal("marker mismatch accepted")
	}
	altered := bytes.Clone(f.Ciphertext)
	altered[len(altered)-1] = 'x'
	if _, err = c.ReplaceIncoming(ctx, f.Source, f.Marker, altered); err == nil {
		t.Fatal("different ciphertext accepted as retry")
	}
	_, err = s.db.Exec("UPDATE messages SET raw=? WHERE id=?", altered, replacement)
	must(t, err)
	if err = c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "read", ""); err == nil {
		t.Fatal("changed ciphertext mutated")
	}
}

func TestNativeIncomingQuotaAndContestedReplacement(t *testing.T) {
	ctx := context.Background()
	_, seed := newTestClient(t)
	seedID := importClient(t, seed.store, "seed", testRaw)
	f, _ := incomingForTest(t, seed, seedID)
	for _, records := range []int{1, 2} {
		t.Run(strconv.Itoa(records), func(t *testing.T) {
			// Only the final ciphertext fits the byte quota; no need to keep two live
			// copies when the transaction commits their replacement atomically.
			limits := Limits{MessageBytes: int64(len(f.Ciphertext)), PayloadBytes: int64(len(f.Ciphertext)), Records: records}
			s := openTest(t, filepath.Join(t.TempDir(), "mailbox"), testOwner, limits)
			c, err := NewClient(s, "alice@example.test")
			must(t, err)
			uid := importClient(t, s, "one", testRaw)
			source, err := c.PrepareIncoming(ctx, uid, false)
			must(t, err)
			replacement, err := c.ReplaceIncoming(ctx, source, f.Marker, f.Ciphertext)
			if records == 1 {
				if !errors.Is(err, ErrCapacity) {
					t.Fatal("record quota failed open")
				}
				raw, e := s.Raw(ctx, "INBOX", int64(uid))
				must(t, e)
				if !bytes.Equal(raw, testRaw) {
					t.Fatal("quota lost original")
				}
			} else {
				must(t, err)
				if replacement <= uid {
					t.Fatal("replacement ID invalid")
				}
			}
		})
	}
	dir := filepath.Join(t.TempDir(), "mailbox")
	s := openTest(t, dir, testOwner, testLimits)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	uid := importClient(t, s, "contest", testRaw)
	source, err := c.PrepareIncoming(ctx, uid, false)
	must(t, err)
	second := openTest(t, dir, testOwner, testLimits)
	other, err := NewClient(second, "alice@example.test")
	must(t, err)
	start := make(chan struct{})
	ids := make(chan int, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, client := range []*Client{c, other} {
		wg.Add(1)
		go func(client *Client) {
			defer wg.Done()
			<-start
			id, e := client.ReplaceIncoming(ctx, source, f.Marker, f.Ciphertext)
			ids <- id
			errs <- e
		}(client)
	}
	close(start)
	wg.Wait()
	first, secondID := <-ids, <-ids
	must(t, <-errs)
	must(t, <-errs)
	if first != secondID || first <= uid {
		t.Fatal("contested replacement duplicated mail")
	}
}

// Kill a real writer after commit but before the caller can persist its result.
// This proves process-crash recovery, not power-loss/hardware fsync behavior.
func TestNativeIncomingCrashHelper(t *testing.T) {
	i := -1
	for j, arg := range os.Args {
		if arg == "--" {
			i = j
			break
		}
	}
	if i < 0 {
		t.Skip("subprocess only")
	}
	root, stage := os.Args[i+1], os.Args[i+2]
	data, err := os.ReadFile(filepath.Join(root, "job.json"))
	must(t, err)
	var f incomingFixture
	must(t, json.Unmarshal(data, &f))
	s, err := Open(filepath.Join(root, "mailbox"), testOwner, testLimits)
	must(t, err)
	c, err := NewClient(s, "alice@example.test")
	must(t, err)
	uid, err := c.ReplaceIncoming(context.Background(), f.Source, f.Marker, f.Ciphertext)
	must(t, err)
	if stage == "move" {
		must(t, c.ApplyIncomingAction(context.Background(), f.Source, uid, f.Marker, f.Ciphertext, "move", "INBOX/Filed"))
	}
	must(t, os.WriteFile(filepath.Join(root, "ready"), []byte(strconv.Itoa(uid)), 0o600))
	select {}
}
func TestNativeIncomingSurvivesKilledWriter(t *testing.T) {
	for _, stage := range []string{"replace", "move"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			s := openTest(t, filepath.Join(root, "mailbox"), testOwner, testLimits)
			c, err := NewClient(s, "alice@example.test")
			must(t, err)
			uid := importClient(t, s, "crash", testRaw)
			f, _ := incomingForTest(t, c, uid)
			data, err := json.Marshal(f)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(root, "job.json"), data, 0o600))
			must(t, s.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeIncomingCrashHelper$", "--", root, stage)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			must(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err = os.Stat(filepath.Join(root, "ready")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("writer not ready: %s", output.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			must(t, cmd.Process.Kill())
			_ = cmd.Wait()
			s = openTest(t, filepath.Join(root, "mailbox"), testOwner, testLimits)
			c, err = NewClient(s, "alice@example.test")
			must(t, err)
			replacement, err := c.ReplaceIncoming(ctx, f.Source, f.Marker, f.Ciphertext)
			must(t, err)
			if stage == "move" {
				must(t, c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "move", "INBOX/Filed"))
				must(t, c.ApplyIncomingAction(ctx, f.Source, replacement, f.Marker, f.Ciphertext, "stop", ""))
			}
			var count int
			must(t, s.db.QueryRow("SELECT count(*) FROM messages WHERE raw IS NOT NULL").Scan(&count))
			if count != 1 {
				t.Fatal("crash replay duplicated/lost copy")
			}
			if _, err = s.Raw(ctx, "INBOX", int64(uid)); !errors.Is(err, ErrNotFound) {
				t.Fatal("plaintext restored after commit")
			}
		})
	}
}
