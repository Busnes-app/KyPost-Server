package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type reencryptFixture struct {
	mu          sync.Mutex
	uidplus     bool
	condstore   bool
	messages    map[int][]byte
	flags       map[int][]string
	modseq      map[int]uint64
	deleted     map[int]bool
	nextUID     int
	appendCount int
	// mutateOnAppend simulates another IMAP session changing flags on uid 7
	// concurrently while ReplaceRawMessageCAS is appending the replacement.
	mutateOnAppend bool
}

func newReencryptFixture(t *testing.T, uidplus, condstore bool) (*APIClient, *reencryptFixture) {
	t.Helper()
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}})
	orig := []byte("From: alice@example.com\r\nSubject: v4\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nv4\r\n-----END PGP MESSAGE-----\r\n")
	f := &reencryptFixture{
		uidplus:   uidplus,
		condstore: condstore,
		messages:  map[int][]byte{7: orig},
		flags:     map[int][]string{7: {`\Seen`}},
		modseq:    map[int]uint64{7: 5},
		deleted:   map[int]bool{},
		nextUID:   8,
	}
	server.mu.Lock()
	server.commandHook = f.respond
	server.appendHook = f.append
	server.mu.Unlock()
	client := server.client("INBOX")
	t.Cleanup(func() { _ = client.Close() })
	return client, f
}

func (f *reencryptFixture) append(_ string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCount++
	uid := f.nextUID
	f.nextUID++
	f.messages[uid] = bytes.Clone(raw)
	f.flags[uid] = []string{`\Seen`}
	f.modseq[uid] = 1
	if f.mutateOnAppend {
		f.mutateOnAppend = false
		f.flags[7] = append(f.flags[7], `\Flagged`)
		f.modseq[7]++
	}
	return nil
}

func (f *reencryptFixture) respond(tag, command string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := tag + " OK done\r\n"
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	switch strings.ToUpper(fields[0]) {
	case "CAPABILITY":
		caps := "IMAP4rev1"
		if f.uidplus {
			caps += " UIDPLUS"
		}
		if f.condstore {
			caps += " CONDSTORE"
		}
		return "* CAPABILITY " + caps + "\r\n" + ok, true
	case "SELECT":
		return ok, true
	case "STATUS":
		return fmt.Sprintf("* STATUS INBOX (UIDNEXT %d)\r\n", f.nextUID) + ok, true
	}
	if len(fields) < 3 || strings.ToUpper(fields[0]) != "UID" {
		return "", false
	}
	switch strings.ToUpper(fields[1]) {
	case "SEARCH":
		var matched []string
		spec := fields[len(fields)-1]
		if strings.HasSuffix(spec, ":*") {
			minUID, _ := strconv.Atoi(strings.TrimSuffix(spec, ":*"))
			for uid := range f.messages {
				if uid >= minUID {
					matched = append(matched, strconv.Itoa(uid))
				}
			}
		} else if uid, err := strconv.Atoi(spec); err == nil {
			if _, exists := f.messages[uid]; exists {
				matched = append(matched, strconv.Itoa(uid))
			}
		}
		return "* SEARCH " + strings.Join(matched, " ") + "\r\n" + ok, true

	case "FETCH":
		uid, _ := strconv.Atoi(fields[2])
		raw, exists := f.messages[uid]
		if !exists {
			return ok, true
		}
		upper := strings.ToUpper(command)
		switch {
		case strings.Contains(upper, "BODY.PEEK[]"):
			return fmt.Sprintf("* 1 FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", uid, len(raw), string(raw)) + ok, true
		case strings.Contains(upper, "MODSEQ"):
			flagList := strings.Join(f.flags[uid], " ")
			return fmt.Sprintf("* 1 FETCH (UID %d FLAGS (%s) MODSEQ (%d))\r\n", uid, flagList, f.modseq[uid]) + ok, true
		default:
			flagList := strings.Join(f.flags[uid], " ")
			return fmt.Sprintf("* 1 FETCH (UID %d FLAGS (%s) INTERNALDATE \"27-Nov-2026 04:00:00 +0000\" RFC822.SIZE %d ENVELOPE (\"Thu, 27 Nov 2026 04:00:00 +0000\" \"v4\" NIL NIL NIL NIL NIL NIL NIL \"<m1@e>\"))\r\n", uid, flagList, len(raw)) + ok, true
		}

	case "STORE":
		uid, _ := strconv.Atoi(fields[2])
		upper := strings.ToUpper(command)
		if strings.Contains(upper, "UNCHANGEDSINCE") {
			idx := strings.Index(upper, "UNCHANGEDSINCE ")
			rest := upper[idx+len("UNCHANGEDSINCE "):]
			end := strings.Index(rest, ")")
			since, _ := strconv.ParseUint(strings.TrimSpace(rest[:end]), 10, 64)
			if f.modseq[uid] > since {
				return fmt.Sprintf("%s OK [MODIFIED %d] Conditional STORE failed\r\n", tag, uid), true
			}
			f.deleted[uid] = true
			f.modseq[uid]++
			flagList := strings.Join(append(append([]string(nil), f.flags[uid]...), `\Deleted`), " ")
			return fmt.Sprintf("* 1 FETCH (UID %d FLAGS (%s) MODSEQ (%d))\r\n", uid, flagList, f.modseq[uid]) + ok, true
		}
		if strings.Contains(upper, `\DELETED`) {
			f.deleted[uid] = true
		}
		return ok, true

	case "EXPUNGE":
		uid, _ := strconv.Atoi(fields[2])
		if f.deleted[uid] {
			delete(f.messages, uid)
			delete(f.flags, uid)
			delete(f.modseq, uid)
			delete(f.deleted, uid)
		}
		return ok, true
	}
	return "", false
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestReplaceRawMessageCASRequiresCondstore(t *testing.T) {
	client, f := newReencryptFixture(t, true, false)
	orig := bytes.Clone(f.messages[7])
	newRaw := []byte("From: alice@example.com\r\nSubject: v6\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nv6\r\n-----END PGP MESSAGE-----\r\n")

	_, err := client.ReplaceRawMessageCAS(context.Background(), "INBOX", 7, sha256Hex(orig), newRaw)
	if !errors.Is(err, ErrCondstoreUnsupported) {
		t.Fatalf("expected ErrCondstoreUnsupported on UIDPLUS-only server, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendCount != 0 {
		t.Fatalf("expected zero appends when CONDSTORE is unavailable, got %d", f.appendCount)
	}
	if !bytes.Equal(f.messages[7], orig) {
		t.Fatalf("original message was modified or removed")
	}
}

func TestReplaceRawMessageCASAbortsOnConcurrentFlagChange(t *testing.T) {
	client, f := newReencryptFixture(t, true, true)
	orig := bytes.Clone(f.messages[7])
	f.mutateOnAppend = true
	newRaw := []byte("From: alice@example.com\r\nSubject: v6\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nv6\r\n-----END PGP MESSAGE-----\r\n")

	_, err := client.ReplaceRawMessageCAS(context.Background(), "INBOX", 7, sha256Hex(orig), newRaw)
	if !errors.Is(err, ErrCASMismatch) {
		t.Fatalf("expected ErrCASMismatch on concurrent flag update, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.messages[7], orig) {
		t.Fatalf("original message must be preserved after concurrent flag change")
	}
	if _, leaked := f.messages[8]; leaked {
		t.Fatalf("appended replacement copy UID 8 was not cleaned up")
	}
}

func TestReplaceRawMessageCASSucceedsWithCondstore(t *testing.T) {
	client, f := newReencryptFixture(t, true, true)
	orig := bytes.Clone(f.messages[7])
	newRaw := []byte("From: alice@example.com\r\nSubject: v6\r\n\r\n-----BEGIN PGP MESSAGE-----\r\nv6\r\n-----END PGP MESSAGE-----\r\n")

	newUID, err := client.ReplaceRawMessageCAS(context.Background(), "INBOX", 7, sha256Hex(orig), newRaw)
	if err != nil {
		t.Fatalf("ReplaceRawMessageCAS failed: %v", err)
	}
	if newUID != 8 {
		t.Fatalf("expected newUID=8, got %d", newUID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, stillThere := f.messages[7]; stillThere {
		t.Fatalf("original UID 7 should have been expunged")
	}
	if !bytes.Equal(f.messages[8], newRaw) {
		t.Fatalf("replacement UID 8 bytes mismatch")
	}
}

func TestListAllFoldersExcludesNoselect(t *testing.T) {
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{
		{name: "INBOX", attrs: `\HasNoChildren`},
		{name: "[Gmail]", attrs: `\Noselect \HasChildren`},
		{name: "Archive", attrs: `\Archive \HasNoChildren`},
		{name: "Ghost", attrs: `\NonExistent`},
	})
	client := server.client("INBOX")
	t.Cleanup(func() { _ = client.Close() })

	got, err := client.ListAllFolders(context.Background())
	if err != nil {
		t.Fatalf("ListAllFolders failed: %v", err)
	}
	want := []string{"Archive", "INBOX"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListAllFolders = %v, want %v", got, want)
	}
}
