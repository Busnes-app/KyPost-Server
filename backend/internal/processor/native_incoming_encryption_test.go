package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var _ incomingEncryptor = (*mailbox.Client)(nil)

// Inject only the missing acknowledgement. All source, replacement, action and
// recovery operations still commit through the real SQLite mailbox Client.
type nativeIncomingLostAck struct {
	*mailbox.Client
	stage  string
	failed bool
}

func (c *nativeIncomingLostAck) ReplaceIncoming(ctx context.Context, source imapadapter.IncomingSource, marker string, cipher []byte) (int, error) {
	uid, err := c.Client.ReplaceIncoming(ctx, source, marker, cipher)
	if err == nil && c.stage == "replace" && !c.failed {
		c.failed = true
		return 0, errors.New("test lost replacement acknowledgement")
	}
	return uid, err
}
func (c *nativeIncomingLostAck) ApplyIncomingAction(ctx context.Context, source imapadapter.IncomingSource, uid int, marker string, cipher []byte, action, value string) error {
	err := c.Client.ApplyIncomingAction(ctx, source, uid, marker, cipher, action, value)
	if err == nil && action == "move" && c.stage == "move" && !c.failed {
		c.failed = true
		return errors.New("test lost move acknowledgement")
	}
	return err
}
func TestNativeIncomingPollerJournalRecovery(t *testing.T) {
	for _, stage := range []string{"replace", "move", "stop-after-move"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			p, uc, _ := incomingPollerForTest(t)
			key, err := pgpmail.GenerateIdentity("Alice", "alice@example.test")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.users.SetPGPIdentityClientProtected(uc.id, key.Fingerprint, key.KeyID, key.ArmoredPublicKey, `{"v":1}`, "generated", "now", nil); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "mailbox")
			owner := mailbox.Owner{Issuer: "https://identity.example.test", Subject: uc.id, Mailbox: uc.id}
			limits := mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100}
			store, err := mailbox.Open(dir, owner, limits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			client, err := mailbox.NewClient(store, "alice@example.test")
			if err != nil {
				t.Fatal(err)
			}
			raw := mailmsg.Message{From: "bob@example.com", To: []string{"alice@example.test"}, Subject: "private subject", Body: "private body", Attachments: []mailmsg.Attachment{{Name: "secret.bin", Content: []byte("original\x00attachment")}}}.Build()
			id, err := store.Import(ctx, mailbox.Receipt{Gateway: "maddy", Delivery: "journal", Sender: "bob@example.com", Recipients: []mailbox.Recipient{{Address: "alice@example.test", Generation: 1}}}, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			faults := &nativeIncomingLostAck{Client: client, stage: stage}
			if stage == "stop-after-move" {
				faults.stage = "move"
			}
			uc.mail = faults
			if stage != "replace" {
				actions := []rules.Action{{Type: "read"}, {Type: "move", Value: "INBOX/Filed"}}
				if stage == "stop-after-move" {
					actions = append(actions, rules.Action{Type: "stop"})
				}
				uc.rules = []rules.Rule{{ID: "file", Enabled: true, Match: rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "from", Comparator: "contains", Value: "bob@example.com"}}}, Actions: actions}}
			}
			var classified atomic.Int32
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Format json.RawMessage }
				if e := json.NewDecoder(r.Body).Decode(&request); e != nil {
					t.Error(e)
				}
				if len(request.Format) != 0 {
					classified.Add(1)
				}
				_, _ = w.Write([]byte(`{"response":"Bills"}`))
			}))
			defer llm.Close()
			p.classifier = classifier.NewHTTPClient(llm.URL, "", "", "", time.Second)
			cache, err := p.userMailCacheStore(uc.id)
			if err != nil {
				t.Fatal(err)
			}
			if err = cache.Upsert("INBOX", []mailcache.Entry{{UID: int(id), MessageID: strconv.FormatInt(id, 10), Subject: "private subject", Body: "private body"}}); err != nil {
				t.Fatal(err)
			}
			messages, _, err := client.ListUnreadInbox(ctx, "")
			if err != nil || len(messages) != 1 {
				t.Fatal("native poll read", err)
			}
			if err = p.handleMessage(ctx, uc, messages[0]); err == nil {
				t.Fatal("lost acknowledgement did not leave pending job")
			}
			journal, err := os.ReadFile(p.incomingJobPath(uc.id))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(journal, []byte("private subject")) || bytes.Contains(journal, []byte("private body")) {
				t.Fatal("journal leaked original plaintext")
			}
			u, err := p.users.Get(uc.id)
			if err != nil || !u.IncomingEncryptionPending {
				t.Fatal("missing pending-key guard", err)
			}
			if _, err = p.users.ClearPGPIdentity(uc.id, &u.PGPRevision); !errors.Is(err, users.ErrIncomingEncryptionPending) {
				t.Fatal("pending job allowed key deletion")
			}
			var job incomingJob
			if err = json.Unmarshal(journal, &job); err != nil {
				t.Fatal(err)
			}
			if job.Source.UID != int(id) || job.Source.Raw != nil || job.Marker == "" || job.Fingerprint != key.Fingerprint {
				t.Fatal("journal source/key binding incomplete")
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := mailbox.Open(dir, owner, limits)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			recovered, err := mailbox.NewClient(reopened, "alice@example.test")
			if err != nil {
				t.Fatal(err)
			}
			// Runtime recovery uses only the durable journal and new store connection,
			// even when the preference is disabled and the copy already moved/read.
			uc.mail = recovered
			uc.encryptIncoming = false
			if err = p.resumeIncomingEncryption(ctx, uc); err != nil {
				t.Fatal(err)
			}
			expectedCalls := int32(1)
			if stage == "stop-after-move" {
				expectedCalls = 0
			}
			if classified.Load() != expectedCalls {
				t.Fatal("recovery reclassified message")
			}
			folder := "INBOX"
			if stage != "replace" {
				folder = "INBOX/Filed"
			}
			copies, err := reopened.List(ctx, folder, 0, 10)
			if err != nil || len(copies) != 1 {
				t.Fatal("missing or duplicate recovered copy", err)
			}
			replacement := copies[0].ID
			if replacement == id || copies[0].Subject != pgpmail.OuterPlaceholderSubject {
				t.Fatal("replacement reused ID or subject")
			}
			content, err := recovered.GetMessageBodies(ctx, folder, []int{int(replacement)})
			if err != nil {
				t.Fatal(err)
			}
			decrypted, err := pgpmail.DecryptMIME(content[int(replacement)].PGPEncryptedPayload, key, nil)
			if err != nil || !bytes.Contains(decrypted.Content, raw) {
				t.Fatal("recovery lost exact original MIME", err)
			}
			for _, processed := range []int64{id, replacement} {
				seen, e := uc.store.Seen(strconv.FormatInt(processed, 10))
				if e != nil || !seen {
					t.Fatal("processed IDs incomplete", e)
				}
			}
			if _, err = os.Stat(p.incomingJobPath(uc.id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed journal retained")
			}
			u, err = p.users.Get(uc.id)
			if err != nil || u.IncomingEncryptionPending {
				t.Fatal("key reservation retained", err)
			}
			decisions := uc.store.Decisions(10)
			if len(decisions) != 1 || decisions[0].MessageID != strconv.FormatInt(replacement, 10) || strings.Contains(decisions[0].Subject, "private") {
				t.Fatal("replacement decision invalid")
			}
			entries, _, err := cache.Snapshot("INBOX", 10)
			if err != nil || len(entries) != 0 {
				t.Fatal("plaintext cache row retained", err)
			}
			if stage != "stop-after-move" && (decisions[0].Label != "Bills" || len(copies[0].Labels) != 1 || copies[0].Labels[0] != "Bills") {
				t.Fatal("classification labels lost")
			}
			// An already-finished recovery is a no-op.
			uc.incomingPending = true
			if err = p.resumeIncomingEncryption(ctx, uc); err != nil {
				t.Fatal(err)
			}
			if len(uc.store.Decisions(10)) != 1 {
				t.Fatal("completed recovery duplicated audit")
			}

		})
	}
}
