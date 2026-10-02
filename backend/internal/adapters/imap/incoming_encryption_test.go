package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type incomingFixture struct {
	mu            sync.Mutex
	messages      map[int][]byte
	validity      uint64
	uidplus       bool
	noMove        bool
	appendCount   int
	loseAppend    bool
	corruptCopy   bool
	closeMetadata bool
	folder        string
	moved         map[int][]byte
}

func newIncomingFixture(t *testing.T) (*APIClient, *incomingFixture, *fakeIMAPServer) {
	t.Helper()
	server := newFakeIMAPServer(t, "/", true, []fakeFolder{{name: "INBOX"}, {name: "Archive"}})
	fixture := &incomingFixture{messages: map[int][]byte{7: []byte("From: alice@example.com\r\nSubject: secret\r\nContent-Type: text/plain\r\n\r\nsecret body")}, validity: 91, uidplus: true, moved: map[int][]byte{}}
	server.mu.Lock()
	server.commandHook = fixture.respond
	server.appendHook = fixture.append
	server.mu.Unlock()
	client := server.client("INBOX")
	t.Cleanup(func() { _ = client.Close() })
	return client, fixture, server
}

func (f *incomingFixture) append(command string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCount++
	f.messages[8] = bytes.Clone(raw)
	if f.corruptCopy {
		f.messages[8] = append(f.messages[8], 'x')
	}
	if f.loseAppend {
		f.loseAppend = false
		return errors.New("lost APPEND acknowledgement")
	}
	return nil
}

func (f *incomingFixture) respond(tag, command string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := tag + " OK done\r\n"
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	switch fields[0] {
	case "CAPABILITY":
		caps := "IMAP4rev1"
		if f.uidplus {
			caps += " UIDPLUS"
			if !f.noMove {
				caps += " MOVE"
			}
		}
		return "* CAPABILITY " + caps + "\r\n" + ok, true
	case "STATUS":
		return fmt.Sprintf("* STATUS INBOX (UIDVALIDITY %d)\r\n", f.validity) + ok, true
	case "SELECT":
		f.folder = strings.Trim(fields[1], `"`)
		return ok, true
	}
	if len(fields) < 3 || fields[0] != "UID" {
		return "", false
	}
	messages := f.messages
	if f.folder == "Archive" {
		messages = f.moved
	}
	switch fields[1] {
	case "SEARCH":
		uids := []string{}
		if strings.Contains(command, " LARGER ") {
			return "* SEARCH\r\n" + ok, true
		}
		switch fields[2] {
		case "HEADER":
			marker := strings.Trim(fields[4], `"`)
			for uid, raw := range messages {
				if bytes.Contains(raw, []byte("X-KyPost-Incoming: "+marker)) {
					uids = append(uids, strconv.Itoa(uid))
				}
			}
		case "UID":
			uid, _ := strconv.Atoi(fields[3])
			if _, exists := messages[uid]; exists {
				uids = append(uids, strconv.Itoa(uid))
			}
		}
		return "* SEARCH " + strings.Join(uids, " ") + "\r\n" + ok, true
	case "FETCH":
		uid, _ := strconv.Atoi(fields[2])
		raw, exists := messages[uid]
		if !exists {
			return ok, true
		}
		if strings.Contains(command, "INTERNALDATE") {
			if f.closeMetadata {
				f.closeMetadata = false
				f.validity++
				return "CLOSE", true
			}
			return fmt.Sprintf("* 1 FETCH (UID %d FLAGS (Bills \\Seen) INTERNALDATE \"02-Oct-2026 12:00:00 +0000\")\r\n", uid) + ok, true
		}
		return fmt.Sprintf("* 1 FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", uid, len(raw), raw) + ok, true
	case "EXPUNGE":
		uid, _ := strconv.Atoi(fields[2])
		delete(messages, uid)
		return ok, true
	case "STORE":
		return ok, true
	case "MOVE":
		uid, _ := strconv.Atoi(fields[2])
		if raw, exists := messages[uid]; exists {
			f.moved[18] = raw
			delete(messages, uid)
		}
		return ok, true
	}
	return "", false
}

func incomingCipherForTest() (string, []byte) {
	marker := strings.Repeat("ab", 32)
	return marker, []byte("X-KyPost-Incoming: " + marker + "\r\nContent-Type: multipart/encrypted\r\n\r\nciphertext")
}

func TestIncomingReplacementVerifiedAndTargeted(t *testing.T) {
	client, f, server := newIncomingFixture(t)
	source, err := client.PrepareIncoming(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if source.UIDValidity != 91 || !source.Date.Equal(source.Date.UTC()) || !bytes.Contains(source.Raw, []byte("secret body")) {
		t.Fatalf("bad source: %#v", source)
	}
	marker, cipher := incomingCipherForTest()
	uid, err := client.ReplaceIncoming(context.Background(), source, marker, cipher)
	if err != nil || uid != 8 {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
	f.mu.Lock()
	_, original := f.messages[7]
	copies := f.appendCount
	f.mu.Unlock()
	if original || copies != 1 {
		t.Fatalf("original=%v copies=%d", original, copies)
	}
	commands := server.commandsMatching("")
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "UID EXPUNGE 7") || strings.Contains(joined, "\nEXPUNGE") {
		t.Fatal(joined)
	}
	if !strings.Contains(joined, `APPEND "INBOX" (Bills \Seen) " 2-Oct-2026 12:00:00 +0000"`) {
		t.Fatal("arrival date/flags not preserved: " + joined)
	}
}

func TestIncomingLostAppendAcknowledgementDoesNotDuplicate(t *testing.T) {
	client, f, _ := newIncomingFixture(t)
	source, err := client.PrepareIncoming(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}
	marker, cipher := incomingCipherForTest()
	f.mu.Lock()
	f.loseAppend = true
	f.mu.Unlock()
	if _, err := client.ReplaceIncoming(context.Background(), source, marker, cipher); err == nil {
		t.Fatal("uncertain append reported success")
	}
	if uid, err := client.ReplaceIncoming(context.Background(), source, marker, cipher); err != nil || uid != 8 {
		t.Fatalf("recovery uid=%d err=%v", uid, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendCount != 1 {
		t.Fatalf("duplicated upload: %d", f.appendCount)
	}
}

func TestIncomingGuardsPreserveOriginal(t *testing.T) {
	for _, scenario := range []string{"namespace", "account", "corrupt-copy", "no-uidplus", "disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			client, f, server := newIncomingFixture(t)
			source, err := client.PrepareIncoming(context.Background(), 7, false)
			if err != nil {
				t.Fatal(err)
			}
			marker, cipher := incomingCipherForTest()
			f.mu.Lock()
			switch scenario {
			case "namespace":
				f.validity++
			case "account":
				source.AccountHash = "wrong"
			case "corrupt-copy":
				f.corruptCopy = true
			case "no-uidplus":
				f.uidplus = false
			case "disconnect":
				f.closeMetadata = true
			}
			f.mu.Unlock()
			if _, err := client.ReplaceIncoming(context.Background(), source, marker, cipher); err == nil {
				t.Fatal("guard did not refuse")
			}
			f.mu.Lock()
			_, original := f.messages[7]
			f.mu.Unlock()
			if !original {
				t.Fatal("original deleted")
			}
			for _, command := range server.commandsMatching("") {
				if strings.HasPrefix(command, "UID STORE") || strings.HasPrefix(command, "UID EXPUNGE") {
					t.Fatalf("mutation after failed guard: %s", command)
				}
			}
			// No library helper may reconnect and carry on in the new namespace.
			if scenario == "disconnect" {
				f.mu.Lock()
				f.messages[7] = []byte("unrelated recycled UID")
				f.mu.Unlock()
				if _, err := client.ReplaceIncoming(context.Background(), source, marker, cipher); err == nil {
					t.Fatal("recycled UID accepted")
				}
			}
		})
	}
}

func TestIncomingActionsVerifyAndRecoverMoves(t *testing.T) {
	client, f, _ := newIncomingFixture(t)
	source, err := client.PrepareIncoming(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}
	marker, cipher := incomingCipherForTest()
	uid, err := client.ReplaceIncoming(context.Background(), source, marker, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ApplyIncomingAction(context.Background(), source, uid, marker, cipher, "move", "Archive"); err != nil {
		t.Fatal(err)
	}
	// Retry models crash between server-side MOVE and journal advancement.
	if err := client.ApplyIncomingAction(context.Background(), source, uid, marker, cipher, "move", "Archive"); err != nil {
		t.Fatalf("move recovery: %v", err)
	}
	f.mu.Lock()
	f.validity++
	f.messages[uid] = []byte("unrelated UID")
	f.mu.Unlock()
	if err := client.ApplyIncomingAction(context.Background(), source, uid, marker, cipher, "keyword", "Bills"); err == nil {
		t.Fatal("unverified recycled UID was labelled")
	}
}

func TestIncomingMoveRequiresCapabilityBeforeReplacement(t *testing.T) {
	client, fixture, server := newIncomingFixture(t)
	fixture.mu.Lock()
	fixture.noMove = true
	fixture.mu.Unlock()
	if _, err := client.PrepareIncoming(context.Background(), 7, true); err == nil {
		t.Fatal("moving rules accepted without MOVE")
	}
	if len(server.commandsMatching("APPEND")) != 0 || len(server.commandsMatching("UID EXPUNGE")) != 0 {
		t.Fatal("mutated original")
	}
}

func TestIncomingMoveCreatesDestinationBeforeGuardedAction(t *testing.T) {
	client, _, server := newIncomingFixture(t)
	source, err := client.PrepareIncoming(context.Background(), 7, true)
	if err != nil {
		t.Fatal(err)
	}
	marker := strings.Repeat("a", 64)
	cipher := []byte("X-KyPost-Incoming: " + marker + "\r\nContent-Type: application/octet-stream\r\n\r\ncipher")
	uid, err := client.ReplaceIncoming(context.Background(), source, marker, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ApplyIncomingAction(context.Background(), source, uid, marker, cipher, "move", "NewFolder"); err != nil {
		t.Fatal(err)
	}
	if len(server.commandsMatching("CREATE")) != 1 {
		t.Fatal("destination not created")
	}
}

func TestIncomingEncryptionRejectsNonInboxPollingMailbox(t *testing.T) {
	_, _, server := newIncomingFixture(t)
	client := server.client("Important")
	defer client.Close()
	if _, err := client.PrepareIncoming(context.Background(), 7, false); err == nil {
		t.Fatal("accepted a UID from a different polling mailbox")
	}
	for _, prefix := range []string{"UID FETCH", "APPEND", "UID STORE", "UID EXPUNGE"} {
		if len(server.commandsMatching(prefix)) != 0 {
			t.Fatalf("wrong mailbox reached %s", prefix)
		}
	}
}

func TestIncomingSearchChecksUIDIntegerBounds(t *testing.T) {
	client, fixture, server := newIncomingFixture(t)
	d, err := client.ensureConnectedLocked()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "-1", "4294967296", "18446744073709551615", "4294967295"} {
		server.mu.Lock()
		server.commandHook = func(tag, command string) (string, bool) {
			if strings.HasPrefix(command, "UID SEARCH") {
				return "* SEARCH " + value + "\r\n" + tag + " OK done\r\n", true
			}
			return fixture.respond(tag, command)
		}
		server.mu.Unlock()
		uids, err := incomingSearch(d, "ALL")
		if value == "4294967295" && strconv.IntSize == 64 {
			if err != nil || len(uids) != 1 || uint64(uids[0]) != uint64(4294967295) {
				t.Fatalf("valid 32-bit UID rejected: %v %v", uids, err)
			}
		} else if err == nil {
			t.Fatalf("unsafe UID %s accepted: %v", value, uids)
		}
	}
}
