package processor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/emersion/go-msgauth/dkim"
)

var errFakeRawFetch = errors.New("fake raw message fetch failure")

// stubSendAsMailClient implements imapadapter.Client by embedding the
// (nil) interface and overriding only the two methods
// checkPendingSendAsAliases calls — any other method call would panic on
// the nil embedded interface, which is fine: it means the code under test
// reached further than this test intended and should be caught, not
// silently no-op'd.
type stubSendAsMailClient struct {
	imapadapter.Client
	searchResults map[string][]imapadapter.Overview // keyed by the searched verification code
	searchErr     error
	rawResults    map[int][]byte
	rawErr        error
	searchCalls   []string
	rawCalls      []int
}

func (c *stubSendAsMailClient) SearchMessages(_ context.Context, _, _, query string, _ int) ([]imapadapter.Overview, error) {
	c.searchCalls = append(c.searchCalls, query)
	if c.searchErr != nil {
		return nil, c.searchErr
	}
	return c.searchResults[query], nil
}

func (c *stubSendAsMailClient) FetchRawMessage(_ context.Context, _ string, uid int) ([]byte, error) {
	c.rawCalls = append(c.rawCalls, uid)
	if c.rawErr != nil {
		return nil, c.rawErr
	}
	return c.rawResults[uid], nil
}

// newTestPollerForSendAs builds a minimal *Poller sufficient to exercise
// checkPendingSendAsAliases: a logger and a stateDir so userStateDir/
// userSendAsStore work, with sendAsStores initialized as New() would do.
func newTestPollerForSendAs(t *testing.T) *Poller {
	t.Helper()
	logger, err := logging.New()
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	return &Poller{
		log:          logger,
		stateDir:     t.TempDir(),
		sendAsStores: map[string]*sendas.Store{},
	}
}

// sendAsAliasesFileForTest mirrors the unexported aliasesFile the sendas
// package persists to disk (see sendas/store.go) — used here only to
// backdate ExpiresAt/FailedAt directly on disk for boundary tests, the same
// technique sendas/store_test.go uses from inside its own package, adapted
// for use from outside the package.
type sendAsAliasesFileForTest struct {
	Aliases []sendas.Alias `json:"aliases"`
}

// backdateSendAsField rewrites a single field (by JSON round-trip through a
// map, so it doesn't matter which of Alias's string fields is targeted) of
// the alias with the given ID, directly on the on-disk send_as_aliases.json
// file, then persists it back.
func backdateSendAsField(t *testing.T, stateDir, userID, aliasID string, mutate func(a *sendas.Alias)) {
	t.Helper()
	path := filepath.Join(stateDir, "users", userID, "send_as_aliases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read send_as_aliases.json: %v", err)
	}
	var f sendAsAliasesFileForTest
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal send_as_aliases.json: %v", err)
	}
	found := false
	for i := range f.Aliases {
		if f.Aliases[i].ID == aliasID {
			mutate(&f.Aliases[i])
			found = true
		}
	}
	if !found {
		t.Fatalf("alias %q not found in send_as_aliases.json", aliasID)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal send_as_aliases.json: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write send_as_aliases.json: %v", err)
	}
}

// TestCheckPendingSendAsAliasesStaysPendingOnNoDKIMSignature covers the
// deterministic, no-DNS-required case: a matched message with no
// DKIM-Signature header at all reliably fails DKIMSignedHeaders without
// any network access, so the alias must stay pending. Real DKIM
// pass/fail crypto outcomes (valid signature, tampered body, wrong domain,
// expired signature) are exhaustively covered in
// internal/adapters/imap/dkim_verify_test.go, which has a test-only seam
// for injecting a fake DNS lookup — this package only exercises the
// raw-fetch-then-verify plumbing (see also the two rawCalls-focused tests
// below), not the crypto itself.
func TestCheckPendingSendAsAliasesStaysPendingOnNoDKIMSignature(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "candidate@example.com", "Candidate")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{
			alias.VerificationCode: {{UID: 1}},
		},
		rawResults: map[int][]byte{
			1: []byte("From: candidate@example.com\r\nSubject: no signature here\r\n\r\nbody\r\n"),
		},
	}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatalf("Get: alias not found")
	}
	if got.Status != "pending" {
		t.Fatalf("Status = %q, want pending", got.Status)
	}
	if len(mail.rawCalls) != 1 || mail.rawCalls[0] != 1 {
		t.Fatalf("rawCalls = %v, want exactly one call for UID 1", mail.rawCalls)
	}
}

// TestCheckPendingSendAsAliasesStaysPendingOnRawFetchError confirms a
// FetchRawMessage error for a matched UID is handled gracefully (logged,
// alias left pending for the next tick) rather than propagating a crash.
func TestCheckPendingSendAsAliasesStaysPendingOnRawFetchError(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "candidate@example.com", "Candidate")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{
			alias.VerificationCode: {{UID: 1}},
		},
		rawErr: errFakeRawFetch,
	}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatalf("Get: alias not found")
	}
	if got.Status != "pending" {
		t.Fatalf("Status = %q, want pending", got.Status)
	}
}

func TestCheckPendingSendAsAliasesStaysPendingOnNoSearchMatch(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "candidate@example.com", "Candidate")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{},
	}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatalf("Get: alias not found")
	}
	if got.Status != "pending" {
		t.Fatalf("Status = %q, want pending", got.Status)
	}
	if len(mail.rawCalls) != 0 {
		t.Fatalf("rawCalls = %d, want 0 (no search match should skip raw fetch)", len(mail.rawCalls))
	}
}

func TestCheckPendingSendAsAliasesMarksExpiredAsFailed(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "candidate@example.com", "Candidate")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backdateSendAsField(t, p.stateDir, userID, alias.ID, func(a *sendas.Alias) {
		a.ExpiresAt = time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	})

	mail := &stubSendAsMailClient{}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatalf("Get: alias not found")
	}
	if got.Status != "failed" {
		t.Fatalf("Status = %q, want failed", got.Status)
	}
	if got.FailedAt == "" {
		t.Fatalf("FailedAt not set")
	}
	if len(mail.searchCalls) != 0 {
		t.Fatalf("searchCalls = %d, want 0 (expired record should never be searched)", len(mail.searchCalls))
	}
}

func TestCheckPendingSendAsAliasesIgnoresNonPendingRecords(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	verified, err := store.Create(userID, "verified@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.MarkVerified(verified.ID); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	failed, err := store.Create(userID, "failed@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.MarkFailed(failed.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	mail := &stubSendAsMailClient{}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	if len(mail.searchCalls) != 0 {
		t.Fatalf("searchCalls = %d, want 0 (non-pending records must not be re-examined)", len(mail.searchCalls))
	}
}

func TestCheckPendingSendAsAliasesSweepsOldFailedRecords(t *testing.T) {
	p := newTestPollerForSendAs(t)
	userID := "user-1"

	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	failed, err := store.Create(userID, "failed@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.MarkFailed(failed.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	backdateSendAsField(t, p.stateDir, userID, failed.ID, func(a *sendas.Alias) {
		a.FailedAt = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	})

	mail := &stubSendAsMailClient{}

	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	if _, ok := must2(store.Get(failed.ID)); ok {
		t.Fatalf("Get: expected record to be swept, still present")
	}
}

// newTestPollerWithUsers extends newTestPollerForSendAs with the pieces the
// PGP-User-ID side of verification needs: a real users store and a
// PGP-private-key path under a temp dir.
func newTestPollerWithUsers(t *testing.T) (*Poller, string) {
	t.Helper()
	p := newTestPollerForSendAs(t)
	configDir := t.TempDir()
	usersStore, err := users.LoadOrMigrate(context.Background(), configDir, filepath.Join(configDir, "admin.env"))
	if err != nil {
		t.Fatalf("users.LoadOrMigrate: %v", err)
	}
	p.users = usersStore

	all, err := usersStore.List()
	if err != nil || len(all) == 0 {
		t.Fatalf("expected a bootstrapped user: %v", err)
	}
	return p, all[0].ID
}

// testDKIMKey signs for every domain in these tests; stubVerifiedDKIM serves
// its public half from a fake DNS, so the real verifier checks real signatures.
var testDKIMKey = sync.OnceValue(func() ed25519.PrivateKey {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return key
})

func stubVerifiedDKIM(t *testing.T) {
	t.Helper()
	txt := "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(testDKIMKey().Public().(ed25519.PublicKey))
	lookup := func(string) ([]string, error) { return []string{txt}, nil }
	prev := dkimSignedHeaders
	dkimSignedHeaders = func(raw []byte, domain string) map[string]string {
		return imapadapter.DKIMSignedHeadersWithLookup(raw, domain, lookup)
	}
	t.Cleanup(func() { dkimSignedHeaders = prev })
}

// signDKIM signs raw for domain over headers, with relaxed header
// canonicalization (the variant whose field-name handling differs from net/mail).
func signDKIM(t *testing.T, raw, domain string, headers ...string) string {
	t.Helper()
	var b strings.Builder
	if err := dkim.Sign(&b, strings.NewReader(raw), &dkim.SignOptions{
		Domain: domain, Selector: "sel", Signer: testDKIMKey(),
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, HeaderKeys: headers,
	}); err != nil {
		t.Fatalf("dkim.Sign: %v", err)
	}
	return b.String()
}

// probeRaw is alias's own probe as the alias domain delivers it back, unsigned.
func probeRaw(alias sendas.Alias) string {
	return "From: " + alias.Email + "\r\nTo: " + alias.Email +
		"\r\nSubject: Verify send-as: " + alias.VerificationCode +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\r\nMessage-ID: " + alias.ProbeMessageID + "\r\n\r\nbody\r\n"
}

// proofHeaders is what a typical signer covers on the probe.
var proofHeaders = []string{"from", "to", "subject", "date", "message-id"}

func signedProbe(t *testing.T, alias sendas.Alias) []byte {
	t.Helper()
	return []byte(signDKIM(t, probeRaw(alias), domainOf(alias.Email), proofHeaders...))
}

// seedPollerPGPIdentity generates a key for email and stores it on userID,
// returning the identity so tests can compare fingerprints.
//
// The sealing path is the helper's own: the poller no longer holds the PGP
// master key, because nothing in it opens a user's private key any more.
func seedPollerPGPIdentity(t *testing.T, p *Poller, userID, name, email string) *pgpmail.Identity {
	t.Helper()
	id, err := pgpmailtest.GenerateIdentity(name, email)
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sealed, err := id.SealPrivateKey(filepath.Join(t.TempDir(), "pgp-private-key.key"))
	if err != nil {
		t.Fatalf("SealPrivateKey: %v", err)
	}
	if _, err := p.users.SetPGPIdentity(userID, id.Fingerprint, id.KeyID, id.ArmoredPublicKey, sealed, "generated", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatalf("SetPGPIdentity: %v", err)
	}
	return id
}

// markVerifiedPendingAlias drives one pending alias all the way through
// checkPendingSendAsAliases's verified branch.
func verifyAliasViaPoller(t *testing.T, p *Poller, userID, email string) sendas.Alias {
	t.Helper()
	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, email, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{
			alias.VerificationCode: {{UID: 1}},
		},
		rawResults: map[int][]byte{1: signedProbe(t, alias)},
	}
	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatal("Get: alias not found after verification")
	}
	if got.Status != "verified" {
		t.Fatalf("Status = %q, want verified", got.Status)
	}
	return got
}

// TestSendAsTickVerifiesWithoutPGPKey confirms the User-ID step is strictly
// additive: a user with no PGP identity at all still gets their alias
// verified rather than having the whole verification fail.
func TestSendAsTickVerifiesWithoutPGPKey(t *testing.T) {
	stubVerifiedDKIM(t)
	p, userID := newTestPollerWithUsers(t)

	verifyAliasViaPoller(t, p, userID, "alice@other.example")

	u, err := p.users.Get(userID)
	if err != nil {
		t.Fatalf("users.Get: %v", err)
	}
	if u.PGPPublicKey != "" {
		t.Fatalf("expected no PGP key to be created, got %q", u.PGPPublicKey)
	}
}

// TestCheckPendingSendAsAliasesRejectsAMessageThatDoesNotCarryTheCode pins the
// property the DKIM checks above are supposed to be protecting: the message
// that satisfies the challenge must actually BE a response to this challenge.
//
// The DKIM gate proves the message came from the alias's domain with a signed,
// exactly-once From equal to the alias. It does not prove the message has
// anything to do with the alias being verified — and the only thing that ever
// consulted the code was the IMAP SEARCH term, answered by a server the account
// holder chose with no ownership check (POST /api/imap/config stores any host).
// So an attacker could answer the search with one genuine, unmodified,
// DKIM-signed message the target once sent them and the alias would verify.
func TestCheckPendingSendAsAliasesRejectsAMessageThatDoesNotCarryTheCode(t *testing.T) {
	stubVerifiedDKIM(t)
	p := newTestPollerForSendAs(t)
	const userID = "user-1"
	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "victim@corp.example", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{
			alias.VerificationCode: {{UID: 1}},
		},
		// Genuine, byte-for-byte unmodified, correctly signed — and utterly
		// unrelated to the challenge.
		rawResults: map[int][]byte{1: []byte(
			"From: victim@corp.example\r\nSubject: Lunch tomorrow?\r\n\r\nbody\r\n")},
	}
	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatal("alias vanished")
	}
	if got.Status == "verified" {
		t.Fatalf("alias verified by a message whose Subject %q never carried code %q",
			"Lunch tomorrow?", alias.VerificationCode)
	}
}

// TestCheckPendingSendAsAliasesRejectsAMessagePredatingTheChallenge closes the
// replay half of the same gap: even once the Subject must carry the code, a
// message that predates the challenge cannot be a response to it.
func TestCheckPendingSendAsAliasesRejectsAMessagePredatingTheChallenge(t *testing.T) {
	stubVerifiedDKIM(t)
	p := newTestPollerForSendAs(t)
	const userID = "user-1"
	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "victim@corp.example", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	old := time.Now().Add(-72 * time.Hour).Format(time.RFC1123Z)
	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{
			alias.VerificationCode: {{UID: 1}},
		},
		rawResults: map[int][]byte{1: []byte(
			"From: victim@corp.example\r\nDate: " + old +
				"\r\nSubject: Re: Verify send-as: " + alias.VerificationCode + "\r\n\r\nbody\r\n")},
	}
	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, ok := must2(store.Get(alias.ID))
	if !ok {
		t.Fatal("alias vanished")
	}
	if got.Status == "verified" {
		t.Fatal("alias verified by a message sent 72h before the challenge existed")
	}
}

// TestRawIsAutoReplyRejectsMailingListRedistribution pins the send-as gate
// against a proving message that is entirely genuine.
//
// A list applying DMARC From-munging rewrites From to the list address so
// alignment holds against its own domain, which means it DKIM-signs that
// rewritten From and the echoed Subject itself. Every other gate in
// checkPendingSendAsAliases then passes on real crypto: real signature, real
// domain, real From, and a Subject that still contains the challenge code
// because the check is a substring match and a "[Tag]" prefix does not disturb
// it. The attacker does not even need the server's probe to reach the list —
// GET /api/mail/send-as returns the code, so they post their own message.
//
// rawIsAutoReply is the only gate that can see this, for the reason it already
// rejects auto-responders: a machine at the alias domain emitting a signed
// message is not a person proving control of the mailbox.
func TestRawIsAutoReplyRejectsMailingListRedistribution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"List-Id", "List-Id: Announce <announce.lists.example.org>\r\n"},
		{"List-Post", "List-Post: <mailto:announce@lists.example.org>\r\n"},
		{"List-Unsubscribe", "List-Unsubscribe: <mailto:announce-leave@lists.example.org>\r\n"},
		{"List-Help", "List-Help: <mailto:announce-help@lists.example.org>\r\n"},
		{"Precedence list", "Precedence: list\r\n"},
		{"Precedence bulk", "Precedence: bulk\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("From: Attacker via Announce <announce@lists.example.org>\r\n" +
				"Subject: [Announce] Verify send-as: kp-deadbeef\r\n" +
				tc.header + "\r\nbody\r\n")
			if !rawIsAutoReply(raw) {
				t.Fatal("a mailing-list redistribution was accepted as proof of mailbox control")
			}
		})
	}

	// An ordinary human reply must still verify, or the gate has eaten the
	// feature it guards.
	ordinary := []byte("From: Alice <alice@other.example>\r\n" +
		"Subject: Re: Verify send-as: kp-deadbeef\r\n\r\nhere you go\r\n")
	if rawIsAutoReply(ordinary) {
		t.Fatal("an ordinary reply was rejected as automated")
	}
}

// A code-confirmed alias is still examined until its window closes, so the
// DKIM loop-back can upgrade it to domain-proven (the only proof WKD accepts).
func TestCheckPendingSendAsAliasesUpgradesCodeConfirmedAlias(t *testing.T) {
	stubVerifiedDKIM(t)
	p, userID := newTestPollerWithUsers(t)
	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "alias@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Confirm(alias.ID, alias.VerificationCode); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{alias.VerificationCode: {{UID: 1}}},
		rawResults:    map[int][]byte{1: signedProbe(t, alias)},
	}
	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, _ := must2(store.Get(alias.ID))
	if got.VerifiedBy != sendas.VerifiedByDKIM || !got.DomainProven() {
		t.Fatalf("after DKIM match: %+v, want upgraded to dkim", got)
	}
}

// Domain proof is the alias's own probe coming back signed: never a reply to
// it (a colleague's out-of-office or answer to a probe the account holder sent
// through their own SMTP), and never on headers the alias domain did not sign.
func TestProvesSendAsRequiresTheSignedProbeItself(t *testing.T) {
	alias := sendas.Alias{
		Email:            "colleague@corp.example",
		VerificationCode: "kp-0123abcd",
		ProbeMessageID:   "<probe123@corp.example>",
		CreatedAt:        time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	now := time.Now().Format(time.RFC1123Z)
	dateLine := regexp.MustCompile("\r\nDate: [^\r]*")
	reply := "From: colleague@corp.example\r\nSubject: Re: Verify send-as: kp-0123abcd" +
		"\r\nDate: " + now + "\r\nMessage-ID: <reply9@corp.example>" +
		"\r\nIn-Reply-To: " + alias.ProbeMessageID + "\r\nReferences: " + alias.ProbeMessageID +
		"\r\n\r\nI am out of the office.\r\n"
	all := []string{"from", "message-id", "subject", "date", "in-reply-to", "references", "auto-submitted"}
	without := func(drop string) []string {
		var out []string
		for _, h := range all {
			if h != drop {
				out = append(out, h)
			}
		}
		return out
	}
	sign := func(raw string, headers ...string) string { return signDKIM(t, raw, "corp.example", headers...) }
	// Genuine signed mail from the address, unrelated to the challenge.
	unrelated := sign("From: colleague@corp.example\r\nSubject: Lunch?\r\nDate: "+now+
		"\r\nMessage-ID: <other7@corp.example>\r\n\r\nbody\r\n", all...)
	stapled := "Message-ID : " + alias.ProbeMessageID + "\r\nSubject : Verify send-as: kp-0123abcd\r\n"
	otherIDProbe := sign(strings.Replace(probeRaw(alias), alias.ProbeMessageID, "<other7@corp.example>", 1), all...)

	stubVerifiedDKIM(t)
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"the signed probe", sign(probeRaw(alias), all...), true},
		{"reply quoting the probe in In-Reply-To", sign(reply, all...), false},
		{"out-of-office with Auto-Submitted stripped", sign(reply, without("auto-submitted")...), false},
		{"probe with Message-ID unsigned", sign(probeRaw(alias), without("message-id")...), false},
		{"probe with Date unsigned", sign(probeRaw(alias), without("date")...), false},
		{"probe with Subject unsigned", sign(probeRaw(alias), without("subject")...), false},
		{"signed From is another address, alias stapled as a variant", "From : colleague@corp.example\r\n" +
			sign(strings.Replace(probeRaw(alias), "From: colleague@", "From: other@", 1), all...), false},
		{"probe without Date", sign(dateLine.ReplaceAllString(probeRaw(alias), ""), all...), false},
		{"probe dated before the challenge", sign(dateLine.ReplaceAllString(probeRaw(alias),
			"\r\nDate: "+time.Now().Add(-72*time.Hour).Format(time.RFC1123Z)), all...), false},
		{"probe from another address", sign(strings.Replace(probeRaw(alias), "From: colleague@", "From: other@", 1), all...), false},
		{"probe marked automated", sign(strings.Replace(probeRaw(alias), "\r\n\r\n", "\r\nAuto-Submitted: auto-replied\r\n\r\n", 1), all...), false},
		{"probe signed by another domain", signDKIM(t, probeRaw(alias), "elsewhere.example", all...), false},
		// Whitespace before the colon: the verifier trims it, net/mail does not.
		{"unrelated signed mail with variant proof headers stapled above", stapled + unrelated, false},
		{"other signed message with variant Message-ID stapled below",
			strings.Replace(otherIDProbe, "\r\n\r\n", "\r\nMessage-ID : "+alias.ProbeMessageID+"\r\n\r\n", 1), false},
		{"signed probe with a duplicate Subject", "Subject: Verify send-as: kp-0123abcd\r\n" + sign(probeRaw(alias), all...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provesSendAs([]byte(tc.raw), alias); got != tc.want {
				t.Fatalf("provesSendAs = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("record without a probe Message-ID", func(t *testing.T) {
		legacy := alias
		legacy.ProbeMessageID = ""
		if provesSendAs([]byte(sign(probeRaw(alias), all...)), legacy) {
			t.Fatal("a record with no recorded probe was proven")
		}
	})
}

// The reviewer's regression, end to end through the poller: genuine signed
// mail from the alias with whitespace-variant proof headers stapled on must
// leave the alias pending.
func TestCheckPendingSendAsAliasesRefusesWhitespaceVariantProofHeaders(t *testing.T) {
	stubVerifiedDKIM(t)
	p, userID := newTestPollerWithUsers(t)
	store, err := p.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "colleague@corp.example", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	genuine := signDKIM(t, "From: colleague@corp.example\r\nSubject: Lunch?\r\nDate: "+
		time.Now().Format(time.RFC1123Z)+"\r\nMessage-ID: <other7@corp.example>\r\n\r\nbody\r\n",
		"corp.example", proofHeaders...)
	// Relaxed canonicalization lets the signed lines be respelled "Name :",
	// which net/mail drops, so it reads only the unsigned plain lines on top.
	respelled := strings.Replace(strings.Replace(genuine,
		"\r\nMessage-ID: <other7@", "\r\nMessage-ID : <other7@", 1),
		"\r\nSubject: Lunch?", "\r\nSubject : Lunch?", 1)
	forged := "Message-ID: " + alias.ProbeMessageID + "\r\nSubject: Verify send-as: " + alias.VerificationCode + "\r\n" + respelled
	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{alias.VerificationCode: {{UID: 1}}},
		rawResults:    map[int][]byte{1: []byte(forged)},
	}
	p.checkPendingSendAsAliases(context.Background(), userID, mail)

	got, _ := must2(store.Get(alias.ID))
	if got.Status != "pending" {
		t.Fatalf("Status = %q, want pending", got.Status)
	}
}

// One address, one key on WKD: a second user cannot domain-prove an address
// another active user already holds proven.
func TestCheckPendingSendAsAliasesRefusesAddressProvenByAnotherUser(t *testing.T) {
	stubVerifiedDKIM(t)
	p, firstID := newTestPollerWithUsers(t)
	verifyAliasViaPoller(t, p, firstID, "Shared@Corp.example")

	second, err := p.users.Create(context.Background(), "second-user", "initial-pass-123", users.RoleUser)
	if err != nil {
		t.Fatalf("Create user: %v", err)
	}
	store, err := p.userSendAsStore(second.ID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(second.ID, "shared@corp.example", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mail := &stubSendAsMailClient{
		searchResults: map[string][]imapadapter.Overview{alias.VerificationCode: {{UID: 1}}},
		rawResults:    map[int][]byte{1: signedProbe(t, alias)},
	}
	p.checkPendingSendAsAliases(context.Background(), second.ID, mail)

	got, _ := must2(store.Get(alias.ID))
	if got.DomainProven() {
		t.Fatalf("second user domain-proved an address another user holds: %+v", got)
	}
}
