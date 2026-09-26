package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
)

// maxSendAsAliasesPerUser bounds how many alias records (pending + verified)
// one account may accumulate, to cap the blast radius of the abuse this
// endpoint could otherwise enable (each Create sends an unsolicited email to
// a third party the caller doesn't necessarily control).
const maxSendAsAliasesPerUser = 20

// handleSendAs serves the caller's own send-as alias list and creates new
// pending aliases (dispatching a probe email to the candidate address).
func (s *Server) handleSendAs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		store, err := s.sendAsFor(r)
		if err != nil {
			http.Error(w, "failed to open send-as store", http.StatusInternalServerError)
			return
		}
		list, err := store.List()
		if err != nil {
			http.Error(w, "failed to read send-as aliases", http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []sendas.Alias{}
		}
		// The code is the proof handleSendAsConfirm checks; serving it here
		// would let any session verify any address without reading its mailbox.
		for i := range list {
			list[i].VerificationCode = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"aliases": list})
	case http.MethodPost:
		s.handleSendAsCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSendAsCreate implements the POST branch of handleSendAs. The
// validation/side-effect ordering below is deliberate — cheap, no-network
// checks reject bad requests before anything touches the rate limiter or the
// network — and must not be reordered.
func (s *Server) handleSendAsCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 1. Validate and normalize the email.
	parsed, err := mail.ParseAddress(req.Email)
	if err != nil {
		http.Error(w, "invalid email address", http.StatusBadRequest)
		return
	}
	normalizedEmail := strings.ToLower(parsed.Address)

	// 2. Resolve the caller.
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	// 3. Load the caller's own IMAP config.
	imapCfg, exists, err := mailmsg.ReadIMAPConfigPayload(s.userIMAPConfigPath(ac.UserID), s.imapConfigKeyPath)
	if err != nil {
		http.Error(w, "failed to read mail credentials", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "imap configuration is required before sending", http.StatusBadRequest)
		return
	}

	// 4. The caller's own account address is allowed here, deliberately.
	//
	// It used to be rejected as redundant — sending as your own address needs
	// no alias. But WKD publication now requires every address to have passed
	// this same challenge (see publishableAddressesAt), because the IMAP
	// username it previously trusted is self-declared and provably nothing.
	// Rejecting the account address would leave users with no way to prove the
	// one address they most need published. One verification mechanism, every
	// address, including your own.

	// 5. Enforce the per-user cap.
	store, err := s.sendAsFor(r)
	if err != nil {
		http.Error(w, "failed to open send-as store", http.StatusInternalServerError)
		return
	}
	existing, err := store.List()
	if err != nil {
		http.Error(w, "failed to read send-as aliases", http.StatusInternalServerError)
		return
	}
	if len(existing) >= maxSendAsAliasesPerUser {
		http.Error(w, "too many send-as aliases for this account", http.StatusBadRequest)
		return
	}

	// 6. Rate limit. One atomic check-and-record: adjacent allowed()/record()
	// calls still left a window in which two concurrent POSTs for the same
	// (user, email) pair both observed "allowed" and both mailed the candidate
	// address, which belongs to a third party who did not ask to hear from this
	// server.
	key := ac.UserID + "|" + normalizedEmail
	if allowed, retryAfter := s.sendAsCooldown.tryConsume(key); !allowed {
		retrySeconds := int(retryAfter.Seconds()) + 1
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":             "too many verification attempts for this address, try again later",
			"retryAfterSeconds": retrySeconds,
		})
		return
	}
	// 7. Create the pending record before attempting to send the probe
	// email. Do not roll this back if sending fails (step 9) — a send
	// failure just means the record sits pending until the daemon's expiry
	// logic marks it failed.
	alias, err := store.Create(ac.UserID, normalizedEmail, strings.TrimSpace(req.DisplayName))
	if err != nil {
		http.Error(w, "failed to create alias", http.StatusInternalServerError)
		return
	}

	// 8. Resolve the SMTP target.
	smtpHost, smtpPort, addr, err := mailmsg.ResolveSMTPTarget(imapCfg)
	if err != nil {
		http.Error(w, "smtp host is not configured", http.StatusBadRequest)
		return
	}

	// 9. Send the probe email, From the alias itself, to the alias — the same
	// header and envelope sender a real send-as message would use, so an
	// upstream that refuses to relay as that address refuses here, before the
	// alias can be verified. The pending record from step 7 stays regardless.
	// Two ways to finish: the daemon's DKIM loop-back check
	// (processor/sendas_check.go) when the alias domain signs and delivers
	// into this inbox, or the user typing the code into handleSendAsConfirm.
	from := mailmsg.SanitizeHeaderValue(normalizedEmail)
	msg := mailmsg.Message{
		From:    from,
		To:      []string{normalizedEmail},
		Subject: "Verify send-as: " + alias.VerificationCode,
		Body: "KyPost is verifying that you can send mail as this address.\r\n\r\n" +
			"Your verification code is: " + alias.VerificationCode + "\r\n\r\n" +
			"Enter it in KyPost under Settings → Mail → Send-As Addresses within 30 minutes. " +
			"If you did not request this, you can ignore it.",
		Mode: "plain",
	}.Build()

	if err := mailmsg.SMTPDeliver(smtpHost, smtpPort, addr, imapCfg.Username, imapCfg.Password, from, []string{normalizedEmail}, msg); err != nil {
		http.Error(w, fmt.Sprintf("failed to send verification email: %s", err), http.StatusBadGateway)
		return
	}

	// 10. Success response.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"id":        alias.ID,
		"status":    alias.Status,
		"expiresAt": alias.ExpiresAt,
	})
}

// handleSendAsConfirm verifies one of the caller's pending aliases with the
// code read from the probe email. Wrong codes are counted by the store and
// the record fails at its attempt cap, so the 32-bit code cannot be searched.
func (s *Server) handleSendAsConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	store, record, ok := s.ownSendAsRecord(w, r)
	if !ok {
		return
	}
	err := store.Confirm(record.ID, req.Code)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "verified"})
	case errors.Is(err, sendas.ErrCodeMismatch):
		http.Error(w, "that code did not match", http.StatusBadRequest)
	case errors.Is(err, sendas.ErrTooManyAttempts):
		http.Error(w, "too many wrong codes; remove the address and verify it again", http.StatusBadRequest)
	case errors.Is(err, sendas.ErrNotPending):
		http.Error(w, "this address is not awaiting a code", http.StatusConflict)
	default:
		http.Error(w, "failed to confirm alias", http.StatusInternalServerError)
	}
}

// ownSendAsRecord resolves {id} to a record owned by the caller, writing the
// error response itself. A record under another account is 404, not 403, so
// the ID space is not enumerable.
func (s *Server) ownSendAsRecord(w http.ResponseWriter, r *http.Request) (*sendas.Store, sendas.Alias, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return nil, sendas.Alias{}, false
	}
	store, err := s.sendAsFor(r)
	if err != nil {
		http.Error(w, "failed to open send-as store", http.StatusInternalServerError)
		return nil, sendas.Alias{}, false
	}
	record, ok, err := store.Get(id)
	if err != nil {
		http.Error(w, "failed to read send-as aliases", http.StatusInternalServerError)
		return nil, sendas.Alias{}, false
	}
	ac, authed := authFromContext(r)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return nil, sendas.Alias{}, false
	}
	if !ok || record.UserID != ac.UserID {
		http.Error(w, "alias not found", http.StatusNotFound)
		return nil, sendas.Alias{}, false
	}
	return store, record, true
}

// handleSendAsByID deletes one of the caller's own send-as alias records.
func (s *Server) handleSendAsByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, record, ok := s.ownSendAsRecord(w, r)
	if !ok {
		return
	}
	if err := store.Delete(record.ID); err != nil {
		http.Error(w, "failed to delete alias", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
