package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type encryptingMailbox struct {
	noopMailClient
	source     imapadapter.IncomingSource
	replaceErr error
	calls      int
	prepared   int
	actions    []string
	cipher     []byte
}

func (m *encryptingMailbox) PrepareIncoming(_ context.Context, _ int, _ bool) (imapadapter.IncomingSource, error) {
	m.prepared++
	return m.source, nil
}
func (m *encryptingMailbox) ReplaceIncoming(_ context.Context, _ imapadapter.IncomingSource, _ string, encrypted []byte) (int, error) {
	m.calls++
	m.cipher = bytes.Clone(encrypted)
	if m.replaceErr != nil {
		return 0, m.replaceErr
	}
	return 8, nil
}
func (m *encryptingMailbox) ApplyIncomingAction(_ context.Context, _ imapadapter.IncomingSource, uid int, _ string, _ []byte, action, value string) error {
	if uid != 8 {
		return errors.New("action used original UID")
	}
	m.actions = append(m.actions, action+":"+value)
	return nil
}

func incomingPollerForTest(t *testing.T) (*Poller, userCtx, *encryptingMailbox) {
	t.Helper()
	mail := &encryptingMailbox{source: imapadapter.IncomingSource{UID: 7, UIDValidity: 91, AccountHash: "account", Hash: "hash", Date: time.Now(), Raw: []byte("From: bob@example.com\r\nTo: alice@example.com\r\nSubject: secret subject\r\nContent-Type: text/plain\r\n\r\nsecret body")}}
	p, _ := newTickTestPoller(t, mail)
	all, err := p.users.List()
	if err != nil || len(all) == 0 {
		t.Fatalf("users: %v", err)
	}
	u := all[0]
	identity, err := pgpmail.GenerateIdentity("Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	u, err = p.users.SetPGPIdentityClientProtected(u.ID, identity.Fingerprint, identity.KeyID, identity.ArmoredPublicKey, `{"v":1}`, "generated", "now", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.globalStore, err = state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.redaction, err = redaction.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := p.userStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	uc := userCtx{id: u.ID, username: u.Username, mail: mail, store: store, encryptIncoming: true, autoLabelEnabled: true, allowlist: []string{"Bills", "Primary"}, keywordMappings: map[string][]string{}, settings: config.UserNotificationSettings{Mode: "none"}}
	return p, uc, mail
}

func TestIncomingClassificationRunsOnceBeforeRecoverableReplacement(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	var classified atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Prompt string          `json:"prompt"`
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload.Format) == 0 {
			_, _ = w.Write([]byte(`{"response":"Bills"}`))
			return
		}
		classified.Add(1)
		if !strings.Contains(payload.Prompt, "secret body") {
			t.Error("classifier did not receive readable body")
		}
		_, _ = w.Write([]byte(`{"response":"Bills"}`))
	}))
	t.Cleanup(llm.Close)
	p.classifier = classifier.NewHTTPClient(llm.URL, "", "", "", time.Second)
	mail.replaceErr = errors.New("lost upload acknowledgement")
	cache, err := p.userMailCacheStore(uc.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Upsert("INBOX", []mailcache.Entry{{UID: 7, MessageID: "7", Subject: "secret subject", Body: "secret body"}}); err != nil {
		t.Fatal(err)
	}
	msg := imapadapter.Message{ID: "7", Subject: "secret subject", Body: "secret body", Sender: "bob@example.com"}
	if err := p.handleMessage(context.Background(), uc, msg); err == nil {
		t.Fatal("uncertain replacement retired original")
	}
	if seen, err := uc.store.Seen("7"); err != nil || seen {
		t.Fatal("failed replacement marked original processed")
	}
	data, err := os.ReadFile(p.incomingJobPath(uc.id))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret body")) || bytes.Contains(data, []byte("secret subject")) {
		t.Fatal("journal persisted original plaintext")
	}
	u, err := p.users.Get(uc.id)
	if err != nil || !u.IncomingEncryptionPending {
		t.Fatal("pending key not reserved")
	}
	if _, err := p.users.ClearPGPIdentity(uc.id, &u.PGPRevision); !errors.Is(err, users.ErrIncomingEncryptionPending) {
		t.Fatalf("discarded pending key: %v", err)
	}
	// Recovery does not need the original to remain UNSEEN, and disabling the
	// preference prevents new jobs but never abandons an admitted replacement.
	uc.encryptIncoming = false
	mail.replaceErr = nil
	if err := p.resumeIncomingEncryption(context.Background(), uc); err != nil {
		t.Fatal(err)
	}
	if classified.Load() != 1 || mail.prepared != 1 || mail.calls != 2 {
		t.Fatalf("classified=%d prepared=%d replaced=%d", classified.Load(), mail.prepared, mail.calls)
	}
	if len(mail.actions) != 1 || mail.actions[0] != "keyword:Bills" {
		t.Fatalf("classification lost: %v", mail.actions)
	}
	for _, id := range []string{"7", "8"} {
		if seen, err := uc.store.Seen(id); err != nil || !seen {
			t.Fatalf("UID %s not processed: %v", id, err)
		}
	}
	if _, err := os.Stat(p.incomingJobPath(uc.id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed journal retained")
	}
	u, err = p.users.Get(uc.id)
	if err != nil || u.IncomingEncryptionPending {
		t.Fatal("reservation not released")
	}
	entries, _, err := cache.Snapshot("INBOX", 1)
	if err != nil || len(entries) != 0 {
		t.Fatal("plaintext original retained in cache")
	}
	decisions := uc.store.Decisions(10)
	if len(decisions) != 1 || decisions[0].Label != "Bills" || decisions[0].Subject != pgpmail.OuterPlaceholderSubject {
		t.Fatalf("decisions: %v", decisions)
	}
}

func TestIncomingRejectsAmbiguousRuleActionsBeforeReplacement(t *testing.T) {
	for _, actions := range [][]rules.Action{
		{{Type: "move", Value: "Archive"}, {Type: "keyword", Value: "Bills"}},
		{{Type: "archive"}, {Type: "delete"}},
	} {
		if err := validateIncomingActions(actions); err == nil {
			t.Fatal("accepted actions after a move")
		}
	}
	if err := validateIncomingActions([]rules.Action{{Type: "keyword", Value: "Bills"}, {Type: "move", Value: "Archive"}, {Type: "stop"}}); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingEncryptionPhishingAuditOmitsSubject(t *testing.T) {
	p, uc, _ := incomingPollerForTest(t)
	msg := imapadapter.Message{ID: "7", Sender: "attacker@example.net", Subject: "secret subject", Body: "kypost://native-pair?sub=victim"}
	if !p.flagAppImpersonation(context.Background(), uc, msg, "alice@example.com") {
		t.Fatal("message not flagged")
	}
	decisions := uc.store.Decisions(10)
	if len(decisions) != 1 || decisions[0].Subject != pgpmail.OuterPlaceholderSubject {
		t.Fatalf("audit retained plaintext: %v", decisions)
	}
}

func TestIncomingEncryptionPreservesContactPresort(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	uc.knownSenders = map[string]bool{"bob@example.com": true}
	msg := imapadapter.Message{ID: "7", Sender: "bob@example.com", Subject: "secret subject", Body: "secret body"}
	// No classifier is installed: a contact must use the existing Primary presort.
	if err := p.handleMessage(context.Background(), uc, msg); err != nil {
		t.Fatal(err)
	}
	if len(mail.actions) != 1 || mail.actions[0] != "keyword:Primary" {
		t.Fatalf("contact presort lost: %v", mail.actions)
	}
}

func TestIncomingEncryptionPreservesHeaderStopRule(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	uc.headers = map[int]map[string][]string{7: {"X-Spam-Flag": {"YES"}}}
	uc.rules = []rules.Rule{{ID: "spam", Enabled: true, Match: rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "header", Header: "X-Spam-Flag", Comparator: "is", Value: "YES"}}}, Actions: []rules.Action{{Type: "keyword", Value: "Spam"}, {Type: "stop"}}}}
	// A matching stop rule avoids classification while still encrypting the mail.
	if err := p.handleMessage(context.Background(), uc, imapadapter.Message{ID: "7", Body: "secret body"}); err != nil {
		t.Fatal(err)
	}
	if len(mail.actions) != 2 || mail.actions[0] != "keyword:Spam" || mail.actions[1] != "stop:" {
		t.Fatalf("header rule lost: %v", mail.actions)
	}
}
