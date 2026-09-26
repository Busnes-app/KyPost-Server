package api

// No real SMTP server is reachable in this test environment (see
// server_mail_pgp_test.go's TestMailSendBlocksSigningWithRevokedIdentity for
// the same precedent). Tests here that need to exercise the handler far
// enough to reach smtpDeliver point the test user's IMAP config at
// SMTPHost: "127.0.0.1", SMTPPort: 1 — a loopback address on a port nothing
// listens on refuses the connection near-instantly (no DNS lookup involved,
// unlike a fabricated hostname) — and assert the resulting 502, while
// separately asserting via the store directly that the pending record was
// still created correctly.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func writeUnreachableSMTPIMAPConfig(t *testing.T, srv *Server, userID, username string) {
	t.Helper()
	srv.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	if err := writeIMAPConfigPayload(srv.userIMAPConfigPath(userID), srv.imapConfigKeyPath, imapConfigPayload{
		Host:     "imap.example.com",
		Port:     993,
		Username: username,
		Password: "pw",
		Mailbox:  "INBOX",
		SMTPHost: "127.0.0.1",
		SMTPPort: 1,
	}); err != nil {
		t.Fatalf("writeIMAPConfigPayload: %v", err)
	}
}

func TestHandleSendAsCreateHappyPath(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	writeUnreachableSMTPIMAPConfig(t, srv, userID, "alice@example.com")

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "bob@example.com", "displayName": "Bob"}, userID)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}

	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	list := must1(store.List())
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	alias := list[0]
	if alias.Status != "pending" {
		t.Fatalf("Status = %q, want pending", alias.Status)
	}
	if alias.Email != "bob@example.com" {
		t.Fatalf("Email = %q, want bob@example.com", alias.Email)
	}
	if alias.DisplayName != "Bob" {
		t.Fatalf("DisplayName = %q, want Bob", alias.DisplayName)
	}
	if alias.VerificationCode == "" {
		t.Fatalf("expected non-empty VerificationCode")
	}
}

// The caller's own account address is now ACCEPTED here, and that reversal is
// deliberate: WKD publication requires every address to have passed this
// challenge (publishableAddressesAt no longer trusts the self-declared IMAP
// username), so rejecting the account address would leave users unable to
// prove the one address they most need published.
func TestHandleSendAsCreateAcceptsOwnAddress(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	writeUnreachableSMTPIMAPConfig(t, srv, userID, "alice@example.com")

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "ALICE@Example.com"}, userID)

	// The probe send itself fails (the test IMAP/SMTP config is unreachable),
	// but the pending alias must exist: the record is created before the probe
	// precisely so a send failure leaves it pending rather than losing it.
	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	if len(must1(store.List())) != 1 {
		t.Fatalf("expected the own-address alias to be created, got %d (status %d, body=%s)",
			len(must1(store.List())), rec.Code, rec.Body.String())
	}
	if got := must1(store.List())[0].Email; got != "alice@example.com" {
		t.Fatalf("alias email = %q, want normalized %q", got, "alice@example.com")
	}
}

func TestHandleSendAsCreateInvalidEmail(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	writeUnreachableSMTPIMAPConfig(t, srv, userID, "alice@example.com")

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "not-an-email"}, userID)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	if len(must1(store.List())) != 0 {
		t.Fatalf("expected no alias created, got %d", len(must1(store.List())))
	}
}

func TestHandleSendAsCreateEnforcesMaxAliasesPerUser(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	// No IMAP config written — the cap check happens before SMTP resolution,
	// so this test should never reach that far.

	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	for i := 0; i < maxSendAsAliasesPerUser; i++ {
		if _, err := store.Create(userID, "filler"+string(rune('a'+i))+"@example.com", ""); err != nil {
			t.Fatalf("Create filler %d: %v", i, err)
		}
	}

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "onemore@example.com"}, userID)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(must1(store.List())) != maxSendAsAliasesPerUser {
		t.Fatalf("len(list) = %d, want %d (no new alias created)", len(must1(store.List())), maxSendAsAliasesPerUser)
	}
}

func TestHandleSendAsCreateRateLimited(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	writeUnreachableSMTPIMAPConfig(t, srv, userID, "alice@example.com")

	first := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "carol@example.com"}, userID)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first: status = %d, want %d; body=%s", first.Code, http.StatusBadGateway, first.Body.String())
	}

	second := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "carol@example.com"}, userID)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status = %d, want %d; body=%s", second.Code, http.StatusTooManyRequests, second.Body.String())
	}
	var body struct {
		RetryAfterSeconds int `json:"retryAfterSeconds"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, second.Body.String())
	}
	if body.RetryAfterSeconds <= 0 {
		t.Fatalf("retryAfterSeconds = %d, want > 0", body.RetryAfterSeconds)
	}

	third := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodPost, "/api/mail/send-as",
		map[string]string{"email": "dave@example.com"}, userID)
	if third.Code != http.StatusBadGateway {
		t.Fatalf("third (different email): status = %d, want %d (not rate limited); body=%s", third.Code, http.StatusBadGateway, third.Body.String())
	}
}

func TestHandleSendAsListScopedToCaller(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	other, err := srv.users.Create(context.Background(), "other-sendas", "pw-other-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create other user: %v", err)
	}

	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	if _, err := store.Create(userID, "mine@example.com", ""); err != nil {
		t.Fatalf("Create mine: %v", err)
	}
	otherStore, err := srv.userSendAsStore(other.ID)
	if err != nil {
		t.Fatalf("userSendAsStore(other): %v", err)
	}
	if _, err := otherStore.Create(other.ID, "theirs@example.com", ""); err != nil {
		t.Fatalf("Create theirs: %v", err)
	}

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodGet, "/api/mail/send-as", nil, userID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Aliases []struct {
			Email string `json:"email"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Aliases) != 1 || resp.Aliases[0].Email != "mine@example.com" {
		t.Fatalf("unexpected aliases: %+v", resp.Aliases)
	}
}

func TestHandleSendAsDeleteHappyPath(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "todelete@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/mail/send-as/"+alias.ID, nil)
	req.SetPathValue("id", alias.ID)
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleSendAsByID)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if _, ok := must2(store.Get(alias.ID)); ok {
		t.Fatalf("expected alias to be deleted")
	}
}

func TestHandleSendAsDeleteRejectsOtherUsersAlias(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	other, err := srv.users.Create(context.Background(), "other-sendas-del", "pw-other-testpassword", users.RoleUser)
	if err != nil {
		t.Fatalf("Create other user: %v", err)
	}
	store, err := srv.userSendAsStore(userID)
	if err != nil {
		t.Fatalf("userSendAsStore: %v", err)
	}
	alias, err := store.Create(userID, "owned-by-user@example.com", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/mail/send-as/"+alias.ID, nil)
	req.SetPathValue("id", alias.ID)
	authRequestAs(srv, req, other.ID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleSendAsByID)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if _, ok := must2(store.Get(alias.ID)); !ok {
		t.Fatalf("expected alias to still exist after cross-user delete attempt")
	}
}

func TestHandleSendAsDeleteNotFound(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/mail/send-as/nope", nil)
	req.SetPathValue("id", "nope")
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleSendAsByID)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func confirmSendAs(srv *Server, userID, id, code string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/mail/send-as/"+id+"/confirm", bytes.NewReader(body))
	req.SetPathValue("id", id)
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.withAuth(srv.handleSendAsConfirm)(rec, req)
	return rec
}

// The list must not hand the browser the very code Confirm checks; with it,
// any session could verify any address without reading its mailbox.
func TestHandleSendAsListHidesVerificationCode(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store := must1(srv.userSendAsStore(userID))
	alias := must1(store.Create(userID, "bob@example.com", ""))

	rec := doJSONAuth(srv, srv.withAuth(srv.handleSendAs), http.MethodGet, "/api/mail/send-as", nil, userID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(alias.VerificationCode)) || bytes.Contains(rec.Body.Bytes(), []byte("verificationCode")) {
		t.Fatalf("list leaks the verification code: %s", rec.Body.String())
	}
}

func TestHandleSendAsConfirm(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store := must1(srv.userSendAsStore(userID))
	alias := must1(store.Create(userID, "bob@example.com", ""))

	if rec := confirmSendAs(srv, userID, alias.ID, "kp-00000000"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong code: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := must2(store.Get(alias.ID)); got.Status != "pending" || got.ConfirmAttempts != 1 {
		t.Fatalf("after one wrong code: status=%q attempts=%d", got.Status, got.ConfirmAttempts)
	}
	if rec := confirmSendAs(srv, userID, alias.ID, " "+alias.VerificationCode+" "); rec.Code != http.StatusOK {
		t.Fatalf("right code: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := must2(store.Get(alias.ID)); got.Status != "verified" || got.VerifiedAt == "" {
		t.Fatalf("after right code: status=%q verifiedAt=%q", got.Status, got.VerifiedAt)
	}
	// Verified is no longer pending: a second confirm is refused.
	if rec := confirmSendAs(srv, userID, alias.ID, alias.VerificationCode); rec.Code != http.StatusConflict {
		t.Fatalf("re-confirm: status = %d, want 409", rec.Code)
	}
}

func TestHandleSendAsConfirmFailsAfterAttemptCap(t *testing.T) {
	srv := newTestServer(t)
	userID := srv.mustBootstrapUserID(t)
	store := must1(srv.userSendAsStore(userID))
	alias := must1(store.Create(userID, "bob@example.com", ""))

	for i := 0; i < 4; i++ {
		if rec := confirmSendAs(srv, userID, alias.ID, "kp-deadbeef"); rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: status = %d, want 400", i+1, rec.Code)
		}
	}
	rec := confirmSendAs(srv, userID, alias.ID, "kp-deadbeef")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too many") {
		t.Fatalf("fifth attempt: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := must2(store.Get(alias.ID)); got.Status != "failed" {
		t.Fatalf("status after cap = %q, want failed", got.Status)
	}
	// The real code no longer works once the record has failed.
	if rec := confirmSendAs(srv, userID, alias.ID, alias.VerificationCode); rec.Code != http.StatusConflict {
		t.Fatalf("after failure: status = %d, want 409", rec.Code)
	}
}

func TestHandleSendAsConfirmRejectsOtherUsersAlias(t *testing.T) {
	srv := newTestServer(t)
	owner := srv.mustBootstrapUserID(t)
	other := must1(srv.users.Create(context.Background(), "other-sendas-confirm", "pw-other-testpassword", users.RoleUser)).ID
	store := must1(srv.userSendAsStore(owner))
	alias := must1(store.Create(owner, "bob@example.com", ""))

	if rec := confirmSendAs(srv, other, alias.ID, alias.VerificationCode); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got, _ := must2(store.Get(alias.ID)); got.Status != "pending" {
		t.Fatalf("status = %q, want pending", got.Status)
	}
}
