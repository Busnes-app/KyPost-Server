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
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/redaction"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type encryptingMailbox struct {
	noopMailClient
	source     imapadapter.IncomingSource
	sources    map[int]imapadapter.IncomingSource
	replaceErr error
	prepareErr error
	msgs       []imapadapter.Message
	calls      int
	prepared   int
	actions    []string
	cipher     []byte
}

func (m *encryptingMailbox) PrepareIncoming(_ context.Context, uid int, _ bool) (imapadapter.IncomingSource, error) {
	m.prepared++
	if source, ok := m.sources[uid]; ok {
		return source, m.prepareErr
	}
	return m.source, m.prepareErr
}
func (m *encryptingMailbox) ListUnreadInbox(ctx context.Context, checkpoint string) ([]imapadapter.Message, string, error) {
	return (&scriptedMailbox{msgs: m.msgs}).ListUnreadInbox(ctx, checkpoint)
}
func (m *encryptingMailbox) ReplaceIncoming(_ context.Context, source imapadapter.IncomingSource, _ string, encrypted []byte) (int, error) {
	m.calls++
	m.cipher = bytes.Clone(encrypted)
	if m.replaceErr != nil {
		return 0, m.replaceErr
	}
	return source.UID + 1, nil
}
func (m *encryptingMailbox) ApplyIncomingAction(_ context.Context, source imapadapter.IncomingSource, uid int, _ string, _ []byte, action, value string) error {
	if uid != source.UID+1 {
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

func TestIncomingPermanentSizeFailureIsBoundedWithoutClassification(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	var classified atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload.Format) != 0 {
			classified.Add(1)
		}
		_, _ = w.Write([]byte(`{"response":"Primary"}`))
	}))
	t.Cleanup(llm.Close)
	p.classifier = classifier.NewHTTPClient(llm.URL, "", "", "", time.Second)
	p.cfg.RateLimits.PerMinute, p.cfg.RateLimits.PerHour = 1, 1
	sibling := mail.source
	sibling.UID = 9
	sibling.Raw = bytes.Clone(sibling.Raw)
	mail.sources = map[int]imapadapter.IncomingSource{9: sibling}
	previous := mailmsg.MaxInboundMessageBytes
	mailmsg.MaxInboundMessageBytes = 4096
	t.Cleanup(func() { mailmsg.MaxInboundMessageBytes = previous })
	mail.source.Raw = append(mail.source.Raw, bytes.Repeat([]byte("uncompressible enough original plaintext 0123456789"), 65)...)
	if int64(len(mail.source.Raw)) >= mailmsg.MaxInboundMessageBytes {
		t.Fatal("original is already oversized")
	}
	mail.msgs = []imapadapter.Message{{ID: "7", Sender: "bob@example.com", Subject: "secret subject", Body: "secret body"}, {ID: "9", Body: "valid sibling", Subject: "valid"}}
	settings := config.DefaultUserSettings()
	settings.EncryptIncoming = true
	if err := config.SaveUserSettings(p.userSettingsPath(uc.id), settings); err != nil {
		t.Fatal(err)
	}
	u, err := p.users.Get(uc.id)
	if err != nil {
		t.Fatal(err)
	}
	for range maxDeferralAttempts - 2 {
		if _, err := uc.store.RecordDeferral("7"); err != nil {
			t.Fatal(err)
		}
	}
	// Feasibility must reject before any model request.
	if err := p.tickUser(u, time.Now()); err != nil {
		t.Fatal(err)
	}
	if seen, _ := uc.store.Seen("7"); seen {
		t.Fatal("retired before bounded attempt limit")
	}
	if checkpointOf(t, uc.store) != "6" {
		t.Fatal("did not hold checkpoint below retry")
	}
	if err := p.tickUser(u, time.Now()); err != nil {
		t.Fatal(err)
	}
	if seen, _ := uc.store.Seen("7"); !seen {
		t.Fatal("permanent failure never retired")
	}
	if seen, _ := uc.store.Seen("9"); !seen {
		t.Fatal("later mail blocked")
	}
	if checkpointOf(t, uc.store) != "9" {
		t.Fatal("checkpoint never advanced after permanent rejection")
	}
	if mail.calls != 1 || mail.prepared != 3 {
		t.Fatalf("unsafe or repeated replacement: %d/%d", mail.calls, mail.prepared)
	}
	if classified.Load() != 1 {
		t.Fatal("poison used quota or valid sibling was not classified exactly once")
	}
	found := false
	for _, d := range uc.store.Decisions(10) {
		if d.MessageID == "7" {
			if d.Subject != pgpmail.OuterPlaceholderSubject {
				t.Fatal("failure audit leaked subject")
			}
			if strings.Contains(d.Detail, "gave up after") && strings.Contains(d.Detail, "original remains plaintext") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no explicit final plaintext-retention failure")
	}
	if _, err := os.Stat(p.incomingJobPath(uc.id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("permanent preflight failure created a job")
	}
}

func TestIncomingTemporaryPreflightFailureKeepsRetryingWithoutClassification(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	mail.prepareErr = errors.New("connection temporarily unavailable")
	mail.msgs = []imapadapter.Message{{ID: "7", Body: "secret body"}}
	settings := config.DefaultUserSettings()
	settings.EncryptIncoming = true
	if err := config.SaveUserSettings(p.userSettingsPath(uc.id), settings); err != nil {
		t.Fatal(err)
	}
	for range maxDeferralAttempts {
		if _, err := uc.store.RecordDeferral("7"); err != nil {
			t.Fatal(err)
		}
	}
	u, err := p.users.Get(uc.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.tickUser(u, time.Now()); err == nil {
		t.Fatal("connection failure not surfaced")
	}
	if seen, _ := uc.store.Seen("7"); seen {
		t.Fatal("temporary failure retired at cap")
	}
	if mail.calls != 0 || mail.prepared != 1 {
		t.Fatal("unexpected classification/replacement")
	}
}

func TestIncomingRejectsUnsafeRuleValuesBeforeJournalAndRecoversAfterCorrection(t *testing.T) {
	for _, action := range []rules.Action{{Type: "keyword", Value: "Follow Up"}, {Type: "unkeyword", Value: "bad\r\nflag"}, {Type: "move", Value: `bad"folder`}} {
		t.Run(action.Type, func(t *testing.T) {
			p, uc, mail := incomingPollerForTest(t)
			uc.rules = []rules.Rule{{ID: "rule", Enabled: true, Match: rules.MatchGroup{Op: "allof", Conditions: []rules.Condition{{Field: "from", Comparator: "contains", Value: "bob"}}}, Actions: []rules.Action{action, {Type: "stop"}}}}
			msg := imapadapter.Message{ID: "7", Sender: "bob@example.com", Body: "secret body"}
			if err := p.handleMessage(context.Background(), uc, msg); !errors.Is(err, errIncomingPermanent) {
				t.Fatalf("not a permanent rejection: %v", err)
			}
			if mail.calls != 0 || mail.prepared != 0 {
				t.Fatal("unsafe values reached IMAP")
			}
			u, err := p.users.Get(uc.id)
			if err != nil || u.IncomingEncryptionPending {
				t.Fatal("unsafe values reserved key")
			}
			if _, err := os.Stat(p.incomingJobPath(uc.id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe journal persisted")
			}
			uc.rules[0].Actions = []rules.Action{{Type: "keyword", Value: "Safe"}, {Type: "stop"}}
			if err := p.handleMessage(context.Background(), uc, msg); err != nil {
				t.Fatal(err)
			}
			if mail.calls != 1 {
				t.Fatal("corrected rule did not recover")
			}
		})
	}
}

func TestIncomingLegacyUnsafeJobFinishesWithoutExecutingUnsafeActions(t *testing.T) {
	p, uc, mail := incomingPollerForTest(t)
	uc.autoLabelEnabled = false
	mail.replaceErr = errors.New("lost append acknowledgment")
	if err := p.handleMessage(context.Background(), uc, imapadapter.Message{ID: "7", Body: "secret body"}); err == nil {
		t.Fatal("expected pending job")
	}
	data, err := os.ReadFile(p.incomingJobPath(uc.id))
	if err != nil {
		t.Fatal(err)
	}
	var job incomingJob
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	job.Actions = []rules.Action{{Type: "keyword", Value: "bad flag"}}
	if err := saveIncomingJob(p.incomingJobPath(uc.id), job); err != nil {
		t.Fatal(err)
	}
	if err := p.resumeIncomingEncryption(context.Background(), uc); err == nil {
		t.Fatal("expected another uncertain replacement")
	}
	data, err = os.ReadFile(p.incomingJobPath(uc.id))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	if job.Decision.Status != "failed" || len(job.Actions) != 0 || !strings.Contains(job.Decision.Detail, "invalid saved rule") {
		t.Fatal("repair audit lost across interrupted recovery")
	}
	mail.replaceErr = nil
	if err := p.resumeIncomingEncryption(context.Background(), uc); err != nil {
		t.Fatal(err)
	}
	for _, action := range mail.actions {
		if strings.Contains(action, "bad flag") {
			t.Fatal("unsafe action executed")
		}
	}
	u, err := p.users.Get(uc.id)
	if err != nil || u.IncomingEncryptionPending {
		t.Fatal("unsafe old job kept key locked")
	}
	decisions := uc.store.Decisions(10)
	if len(decisions) != 1 || decisions[0].Status != "failed" || !strings.Contains(decisions[0].Detail, "invalid saved rule") {
		t.Fatalf("recovery not reported: %v", decisions)
	}
}
