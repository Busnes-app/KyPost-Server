package processor

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
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const sealMarker = "merger-terms-91c4"

var sealRaw = []byte("From: bob@example.com\r\nTo: alice@example.com\r\nSubject: " + sealMarker + "\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n\r\n" + strings.Repeat(sealMarker+" body line\r\n", 300))

func isSealed(raw []byte) bool {
	parsed, err := imapadapter.ParseRawContent(raw)
	return err == nil && parsed.Content.PGPEncrypted && !bytes.Contains(raw, []byte(sealMarker))
}

// The seal encrypts to the owner's key, keeps already-encrypted mail, refuses an owner it
// cannot read, stores as received when the policy is off or there is no usable key.
func TestIncomingSeal(t *testing.T) {
	p, uc, _ := incomingPollerForTest(t)
	const issuer = "https://identity.example.test"
	if err := p.users.LinkSSOIssuer(issuer, uc.id, "alice-sub", "alice", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seal := IncomingSeal(p.users, p.stateDir, p.globalStore)
	ctx := context.Background()
	owner := mailbox.Owner{Issuer: issuer, Subject: "alice-sub", Mailbox: uc.id}

	sealed, err := seal(ctx, owner, sealRaw)
	if err != nil || !isSealed(sealed) {
		t.Fatalf("keyed owner's mail not sealed: %v", err)
	}
	if again, err := seal(ctx, owner, sealed); err != nil || !bytes.Equal(again, sealed) {
		t.Fatalf("already encrypted mail re-wrapped: %v", err)
	}
	if _, err := seal(ctx, mailbox.Owner{Issuer: issuer, Subject: "nobody", Mailbox: "x"}, sealRaw); err == nil {
		t.Fatal("an unreadable owner was not refused")
	}
	if got, err := sealTo(users.User{}, sealRaw, nil); !errors.Is(err, errNoIncomingKey) || got != nil {
		t.Fatalf("keyless account: %v", err)
	}
	if err := p.globalStore.SetSetting(state.EncryptionPolicySetting, `{"key":"required","incoming":"off"}`); err != nil {
		t.Fatal(err)
	}
	if got, err := seal(ctx, owner, sealRaw); err != nil || !bytes.Equal(got, sealRaw) {
		t.Fatalf("policy off still sealed: %v", err)
	}
}

// Mail stored while the owner had no key is sealed in place once they have one: a
// bounded batch a tick, resuming from the cursor, across folders, idempotent, and with
// nothing left in plaintext in the rows or the files.
func TestSealBacklog(t *testing.T) {
	p, uc, _ := incomingPollerForTest(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "mailbox")
	owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: uc.id, Mailbox: uc.id}
	store, err := mailbox.Open(dir, owner, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 8 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client, err := mailbox.NewClient(store, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, folder := range []string{"INBOX", "INBOX", "Archive"} {
		if folder == "INBOX" {
			id, err := store.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: string(rune('a' + i)), Sender: "bob@example.com", Recipients: []mailbox.Recipient{{Address: "alice@example.com", Generation: 1}}}, bytes.NewReader(sealRaw))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
			continue
		}
		if err := client.ImportMessage(ctx, folder, sealRaw, mailbox.ImportMeta{}); err != nil {
			t.Fatal(err)
		}
		refs, _ := client.MessagesAfter(ctx, ids[len(ids)-1], 1)
		ids = append(ids, refs[0].ID)
	}
	uc.mail, uc.mailbox, uc.primary = client, uc.id, uc.store
	cache, err := mailcache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sealedCount := func() (n int) {
		refs, err := client.MessagesAfter(ctx, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range refs {
			raw, err := client.GetRawMessage(ctx, r.Folder, int(r.ID))
			if err != nil {
				t.Fatal(err)
			}
			if isSealed(raw) {
				n++
			}
		}
		return n
	}
	backlogBatch = 2
	t.Cleanup(func() { backlogBatch = 100 })
	for want := range []int{2, 3, 3} {
		if err := p.sealBacklog(ctx, uc, cache); err != nil {
			t.Fatal(err)
		}
		if got := sealedCount(); got != []int{2, 3, 3}[want] {
			t.Fatalf("tick %d sealed %d", want, got)
		}
	}
	before, err := client.GetRawMessage(ctx, "Archive", int(ids[2]))
	if err != nil {
		t.Fatal(err)
	}
	// A rescan from zero (a new source) skips sealed mail: the bytes stay as they are.
	if err := os.Remove(filepath.Join(p.userStateDir(uc.id), "sealed-backlog-"+uc.id+".json")); err != nil {
		t.Fatal(err)
	}
	if err := p.sealBacklog(ctx, uc, cache); err != nil {
		t.Fatal(err)
	}
	if after, _ := client.GetRawMessage(ctx, "Archive", int(ids[2])); !bytes.Equal(before, after) {
		t.Fatal("sweep re-encrypted sealed mail")
	}
	list, err := store.List(ctx, "INBOX", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if strings.Contains(m.Subject, sealMarker) {
			t.Fatal("plaintext subject left in the row")
		}
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(sealMarker)) {
			t.Errorf("plaintext left in %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// With the admin policy off the sweep leaves mail as received.
func TestSealBacklogPolicyOff(t *testing.T) {
	p, uc, _ := incomingPollerForTest(t)
	ctx := context.Background()
	store, err := mailbox.Open(filepath.Join(t.TempDir(), "mailbox"), mailbox.Owner{Issuer: "https://identity.example.test", Subject: uc.id, Mailbox: uc.id}, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 8 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client, err := mailbox.NewClient(store, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: "k", Sender: "bob@example.com", Recipients: []mailbox.Recipient{{Address: "alice@example.com", Generation: 1}}}, bytes.NewReader(sealRaw))
	if err != nil {
		t.Fatal(err)
	}
	uc.mail, uc.mailbox, uc.primary = client, uc.id, uc.store
	cache, _ := mailcache.New(t.TempDir())
	if err := p.globalStore.SetSetting(state.EncryptionPolicySetting, `{"key":"required","incoming":"off"}`); err != nil {
		t.Fatal(err)
	}
	if err := p.sealBacklog(ctx, uc, cache); err != nil {
		t.Fatal(err)
	}
	if raw, _ := client.GetRawMessage(ctx, "INBOX", int(id)); !bytes.Equal(raw, sealRaw) {
		t.Fatal("swept with the policy off")
	}
}
