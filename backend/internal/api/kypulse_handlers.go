package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/ky-primitives/syncauth"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// KyPulse alert mail (docs/KYPULSE.md).
const (
	kypulseEventAlert     = "kypulse.alert.send"
	maxKyPulseBodyBytes   = 16 << 10
	maxKyPulseRecipients  = 11
	maxKyPulseSubject     = 150
	maxKyPulseText        = 8 << 10
	kypulseAlertsPerHour  = 30
	kypulseSubjectPrefix  = "[KyPulse] "
	kypulseConfirmSubject = "Confirm alert mail"
	kypulseConfirmTTL     = 7 * 24 * time.Hour
)

// KyPulsePeer is the pairing KyPost keeps with KyPulse: KyIdentity only.
var KyPulsePeer = suitepair.Peer{ID: "kypulse", Name: "KyPulse"}

// SetKyPulse pairs KyPulse. Startup-only; nil or unpaired leaves it off.
func (s *Server) SetKyPulse(c *suitepair.Client) { s.kypulse = c }

var errNoUser = refuse(http.StatusNotFound, "no_user", "no active KyPost account for this subject")

// handleKyPulseAlert sends one plain-text alert from a member's mailbox to
// recipients they allowed. Signature only; every refusal is a contract code.
func (s *Server) handleKyPulseAlert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.kypulse.Available() {
		writeITIP(w, http.StatusNotFound, "kypulse_off", "KyPulse is not paired with this server")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxKyPulseBodyBytes+1))
	if err != nil || len(body) > maxKyPulseBodyBytes {
		writeITIP(w, http.StatusBadRequest, "bad_body", "request body unreadable or too large")
		return
	}
	ev, err := s.kypulse.Verify(r, kypulseEventAlert, body)
	if errors.Is(err, syncauth.ErrReplay) {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "duplicate"})
		return
	}
	if err != nil {
		s.logger.Info("kypulse request refused", "reason", err.Error())
		writeITIP(w, http.StatusUnauthorized, "bad_signature", "Bad signature")
		return
	}
	if f := s.sendKyPulseAlert(r.Context(), ev.ID, body); f != nil {
		if !f.keep {
			s.kypulse.Forget(ev.ID)
		}
		if f.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(f.retryAfter))
		}
		s.logger.Info("kypulse alert refused", "event_id", ev.ID, "result", f.code)
		writeITIP(w, f.status, f.code, f.msg)
		return
	}
	s.logger.Info("kypulse alert sent", "event_id", ev.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"outcome": "sent"})
}

func (s *Server) sendKyPulseAlert(ctx context.Context, eventID string, body []byte) *itipRefusal {
	var req struct {
		Sub        string   `json:"sub"`
		Issuer     string   `json:"issuer"`
		Recipients []string `json:"recipients"`
		Subject    string   `json:"subject"`
		Text       string   `json:"text"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	badBody := refuse(http.StatusBadRequest, "bad_body", "malformed request, an unknown or missing field, or a field outside its rule")
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || req.Sub == "" || req.Issuer == "" ||
		!kypulseText(req.Subject, maxKyPulseSubject, false) || !kypulseText(req.Text, maxKyPulseText, true) ||
		len(req.Recipients) == 0 || len(req.Recipients) > maxKyPulseRecipients {
		return badBody
	}
	var recipients []string
	for _, raw := range req.Recipients {
		addr := bareAddress(raw)
		if addr == "" || slices.Contains(recipients, addr) {
			return badBody
		}
		recipients = append(recipients, addr)
	}

	u, err := s.users.GetBySSOSubIssuer(req.Issuer, req.Sub)
	switch {
	case errors.Is(err, users.ErrNotFound) || errors.Is(err, users.ErrNativeAccountConflict):
		return errNoUser
	case err != nil:
		return errUnavailable
	// Dedicated administrator identities have no mailbox in native mode.
	case !u.Active || u.SSOLinkRevokedAt != 0 || s.nativeMail && u.Role == users.RoleAdmin:
		return errNoUser
	}
	out, f := s.kypulseOutbox(ctx, u)
	if f != nil {
		return f
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		return errUnavailable
	}
	if !settings.KyPulse.Enabled {
		return refuse(http.StatusConflict, "alerts_off", "the owner has not let KyPulse send alerts from their mailbox")
	}
	listed := func(addr string) (config.KyPulseRecipient, bool) {
		i := slices.IndexFunc(settings.KyPulse.Recipients, func(x config.KyPulseRecipient) bool { return x.Address == addr })
		if i < 0 {
			return config.KyPulseRecipient{}, false
		}
		return settings.KyPulse.Recipients[i], true
	}
	for _, rcpt := range recipients {
		if _, ok := listed(rcpt); !ok && !slices.Contains(out.own, rcpt) {
			return refuse(http.StatusConflict, "recipient_not_allowed", "a recipient is not on the owner's list")
		}
	}
	for _, rcpt := range recipients {
		if x, ok := listed(rcpt); ok && !x.Confirmed && !slices.Contains(out.own, rcpt) {
			return refuse(http.StatusConflict, "recipient_unconfirmed", "a recipient has not confirmed")
		}
	}
	if out.send == nil {
		return refuse(http.StatusConflict, "no_mailbox", "no way to send from the owner's mailbox")
	}
	// The per-mailbox limit lives in the shared send ledger under its own
	// key, keyed by the event ID so KyPulse's retries count once.
	if s.globalStore == nil {
		return errUnavailable
	}
	wait, err := s.globalStore.AdmitSend(ctx, "kypulse-rate:"+u.ID+":"+eventID, "kypulse:"+u.ID, 1, kypulseAlertsPerHour, kypulseAlertsPerHour*24, time.Now().Unix())
	switch {
	case errors.Is(err, state.ErrSendQuota):
		f := refuse(http.StatusTooManyRequests, "rate_limited", "over the per-mailbox alert limit")
		f.retryAfter = max(1, int(wait.Seconds()))
		return f
	case err != nil:
		return errUnavailable
	}
	return s.sendKyPulseMail(ctx, u, out, "kypulse:"+u.ID+":"+eventID, recipients, req.Subject, req.Text)
}

// kypulseText reports whether v is 1 to maxBytes of UTF-8 with no control
// characters, other than \n when newlines is set.
func kypulseText(v string, maxBytes int, newlines bool) bool {
	return v != "" && len(v) <= maxBytes && utf8.ValidString(v) &&
		!strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) && (r != '\n' || !newlines) })
}

// kypulseOutbox is how KyPulse mail leaves an owner's mailbox: their own
// addresses, the From and the sender; send is nil when there is no way to
// send (no outgoing server, native mail off, no relay).
type kypulseOutbox struct {
	own  []string
	from string
	send func(ctx context.Context, recipients []string, wire, sent []byte) *itipRefusal
}

func (s *Server) kypulseOutbox(ctx context.Context, u users.User) (kypulseOutbox, *itipRefusal) {
	var out kypulseOutbox
	addOwn := func(addr string) {
		if addr := bareAddress(addr); addr != "" && !slices.Contains(out.own, addr) {
			out.own = append(out.own, addr)
		}
	}
	a, native, err := s.nativeMailboxAssignment(ctx, u.ID, "")
	switch {
	case errors.Is(err, sso.ErrNativeAdministrator):
		return out, errNoUser
	case errors.Is(err, sso.ErrNativeMailDisabled):
	case err != nil:
		return out, errUnavailable
	case native:
		addOwn(a.Address)
		boxes, err := s.ssoLifecycle.NativeMailboxesOf(u.ID)
		if err != nil {
			return out, errUnavailable
		}
		for _, m := range boxes {
			if m.User != u.ID || m.Kind == "extra" && (m.State != "active" || !m.Prepared) {
				continue
			}
			for _, x := range m.Addresses {
				if x.State == "active" {
					addOwn(x.Address)
				}
			}
		}
		mailboxID, f := s.nativeITIPMailbox(ctx, u.ID, a, a.Address)
		if f == errUnavailable {
			return out, f
		}
		if f == nil {
			out.from = a.Address
			out.send = func(ctx context.Context, recipients []string, wire, sent []byte) *itipRefusal {
				return s.sendNativeITIP(ctx, "kypulse", u, mailboxID, a.Address, recipients, wire, sent)
			}
		}
	default:
		payload, exists, err := s.outboundMailConfig(u.ID)
		if err != nil {
			return out, errUnavailable
		}
		accountAddr := sanitizeHeaderValue(payload.Username)
		addOwn(accountAddr)
		smtpHost, smtpPort, addr, err := mailmsg.ResolveSMTPTarget(payload)
		if exists && err == nil && bareAddress(accountAddr) != "" {
			out.from = accountAddr
			out.send = func(ctx context.Context, recipients []string, wire, sent []byte) *itipRefusal {
				if err := mailmsg.SMTPDeliver(smtpHost, smtpPort, addr, payload.Username, payload.Password, accountAddr, recipients, wire); err != nil &&
					!errors.Is(err, mailmsg.ErrSMTPAcceptedThenFailed) {
					f := refuse(http.StatusServiceUnavailable, "smtp_failed", "the owner's mail server did not accept the message; retry later")
					f.keep = errors.Is(err, mailmsg.ErrSMTPAcceptanceUncertain)
					return f
				}
				if mailClient, err := s.userMailClient(u.ID); err == nil {
					if err := mailClient.SaveSent(ctx, imapadapter.DraftMessage{To: recipients, Raw: sent}); err != nil {
						s.logger.Error("kypulse send not saved to Sent", "actor", u.ID)
					}
				}
				return nil
			}
		}
	}
	store, err := s.userSendAsStore(u.ID)
	if err != nil {
		return out, errUnavailable
	}
	aliases, err := store.ListVerified()
	if err != nil {
		return out, errUnavailable
	}
	for _, alias := range aliases {
		addOwn(alias.Email)
	}
	return out, nil
}

// sendKyPulseMail charges the owner's sending quota once per chargeID and
// sends the fixed template: plain text, "[KyPulse] " subject, the two
// auto-generated headers, From the owner's own address.
func (s *Server) sendKyPulseMail(ctx context.Context, u users.User, out kypulseOutbox, chargeID string, recipients []string, subject, text string) *itipRefusal {
	wait, err := s.chargeSend(ctx, chargeID, u.ID, len(recipients), time.Now())
	switch {
	case errors.Is(err, state.ErrSendQuota):
		f := refuse(http.StatusTooManyRequests, "rate_limited", "the owner's sending quota is reached")
		f.retryAfter = max(1, int(wait.Seconds()))
		return f
	case errors.Is(err, state.ErrSendTooLarge):
		f := refuse(http.StatusTooManyRequests, "rate_limited", "more recipients than the owner's daily quota")
		f.retryAfter = int(time.Hour.Seconds())
		return f
	case err != nil:
		return errUnavailable
	}
	wire, sent, _ := composeSend(mailmsg.Message{
		From:       out.from,
		To:         recipients,
		Subject:    kypulseSubjectPrefix + subject,
		Body:       strings.ReplaceAll(text, "\n", "\r\n"),
		Mode:       "plain",
		AutoSource: "kypulse",
	}, nil)
	return out.send(ctx, recipients, wire, sent)
}

// kypulseView is the settings answer. available: KyPulse is paired.
func (s *Server) kypulseView(ctx context.Context, userID string) (map[string]any, *itipRefusal) {
	u, err := s.users.Get(userID)
	if err != nil {
		return nil, errUnavailable
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		return nil, errUnavailable
	}
	out, f := s.kypulseOutbox(ctx, u)
	if f != nil && f != errNoUser {
		return nil, f
	}
	recipients := []map[string]any{}
	for _, x := range settings.KyPulse.Recipients {
		recipients = append(recipients, map[string]any{"address": x.Address, "confirmed": x.Confirmed})
	}
	return map[string]any{
		"available":  s.kypulse.Available(),
		"enabled":    settings.KyPulse.Enabled,
		"own":        append([]string{}, out.own...),
		"recipients": recipients,
	}, nil
}

func (s *Server) writeKyPulseView(w http.ResponseWriter, r *http.Request, userID string) {
	v, f := s.kypulseView(r.Context(), userID)
	if f != nil {
		writeITIP(w, f.status, f.code, f.msg)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleKyPulseSettings reads the caller's KyPulse switch and list, or sets
// the switch (PUT {enabled}).
func (s *Server) handleKyPulseSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	if r.Method == http.MethodPut {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req) != nil || req.Enabled == nil {
			writeITIP(w, http.StatusBadRequest, "bad_request", "invalid request")
			return
		}
		if err := config.UpdateUserSettings(s.userSettingsPath(ac.UserID), func(st *config.UserSettings) error {
			st.KyPulse.Enabled = *req.Enabled
			return nil
		}); err != nil {
			writeITIP(w, http.StatusServiceUnavailable, "unavailable", "settings could not be saved")
			return
		}
		result := "off"
		if *req.Enabled {
			result = "on"
		}
		s.logger.Info("kypulse alerts switched", "actor", ac.UserID, "result", result)
	}
	s.writeKyPulseView(w, r, ac.UserID)
}

// kypulseRecipientRequest reads {address} as a bare address.
func kypulseRecipientRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Address string `json:"address"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req) != nil {
		writeITIP(w, http.StatusBadRequest, "bad_request", "invalid request")
		return "", false
	}
	addr := bareAddress(req.Address)
	if addr == "" {
		writeITIP(w, http.StatusBadRequest, "bad_address", "enter a bare email address")
		return "", false
	}
	return addr, true
}

var (
	errKyPulseDuplicate = errors.New("already listed")
	errKyPulseFull      = errors.New("list full")
	errKyPulseMissing   = errors.New("not listed")
	errKyPulseConfirmed = errors.New("already confirmed")
)

// handleKyPulseRecipientAdd lists an extra address and mails it a
// confirmation link from the owner's mailbox.
func (s *Server) handleKyPulseRecipientAdd(w http.ResponseWriter, r *http.Request) {
	s.mailKyPulseConfirmation(w, r, "add", func(st *config.UserSettings, addr, nonce string, now int64) error {
		if slices.ContainsFunc(st.KyPulse.Recipients, func(x config.KyPulseRecipient) bool { return x.Address == addr }) {
			return errKyPulseDuplicate
		}
		if len(st.KyPulse.Recipients) >= config.MaxKyPulseRecipients {
			return errKyPulseFull
		}
		st.KyPulse.Recipients = append(st.KyPulse.Recipients, config.KyPulseRecipient{Address: addr, Nonce: nonce, SentAt: now})
		return nil
	})
}

// handleKyPulseRecipientResend mails a fresh link; the old one stops working.
func (s *Server) handleKyPulseRecipientResend(w http.ResponseWriter, r *http.Request) {
	s.mailKyPulseConfirmation(w, r, "resend", func(st *config.UserSettings, addr, nonce string, now int64) error {
		i := slices.IndexFunc(st.KyPulse.Recipients, func(x config.KyPulseRecipient) bool { return x.Address == addr })
		switch {
		case i < 0:
			return errKyPulseMissing
		case st.KyPulse.Recipients[i].Confirmed:
			return errKyPulseConfirmed
		}
		st.KyPulse.Recipients[i].Nonce, st.KyPulse.Recipients[i].SentAt = nonce, now
		return nil
	})
}

// handleKyPulseRecipientRemove unlists an address, which revokes its link.
func (s *Server) handleKyPulseRecipientRemove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	addr, ok := kypulseRecipientRequest(w, r)
	if !ok {
		return
	}
	err := config.UpdateUserSettings(s.userSettingsPath(ac.UserID), func(st *config.UserSettings) error {
		n := len(st.KyPulse.Recipients)
		st.KyPulse.Recipients = slices.DeleteFunc(st.KyPulse.Recipients, func(x config.KyPulseRecipient) bool { return x.Address == addr })
		if len(st.KyPulse.Recipients) == n {
			return errKyPulseMissing
		}
		return nil
	})
	if !s.kypulseListError(w, ac.UserID, "remove", err) {
		return
	}
	s.logger.Info("kypulse recipient changed", "actor", ac.UserID, "action", "remove", "result", "ok")
	s.writeKyPulseView(w, r, ac.UserID)
}

// kypulseListError answers a failed list change; it reports whether err is nil.
func (s *Server) kypulseListError(w http.ResponseWriter, actor, action string, err error) bool {
	if err == nil {
		return true
	}
	code, status, msg := "unavailable", http.StatusServiceUnavailable, "settings could not be saved"
	switch {
	case errors.Is(err, errKyPulseDuplicate):
		code, status, msg = "duplicate", http.StatusConflict, "that address is already listed"
	case errors.Is(err, errKyPulseFull):
		code, status, msg = "list_full", http.StatusConflict, "at most 10 extra addresses"
	case errors.Is(err, errKyPulseMissing):
		code, status, msg = "not_listed", http.StatusNotFound, "that address is not listed"
	case errors.Is(err, errKyPulseConfirmed):
		code, status, msg = "already_confirmed", http.StatusConflict, "that address has already confirmed"
	}
	s.logger.Info("kypulse recipient changed", "actor", actor, "action", action, "result", code)
	writeITIP(w, status, code, msg)
	return false
}

// mailKyPulseConfirmation stores a new nonce for an address through change
// and mails the address a link bound to it, from the owner's mailbox.
func (s *Server) mailKyPulseConfirmation(w http.ResponseWriter, r *http.Request, action string, change func(st *config.UserSettings, addr, nonce string, now int64) error) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	addr, ok := kypulseRecipientRequest(w, r)
	if !ok {
		return
	}
	if !s.pairingSecretConfigured() {
		writeITIP(w, http.StatusServiceUnavailable, "links_off", "confirmation links are not configured on this server")
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "account unavailable")
		return
	}
	out, f := s.kypulseOutbox(r.Context(), u)
	switch {
	case f == errNoUser || f == nil && out.send == nil:
		writeITIP(w, http.StatusConflict, "no_mailbox", "this account has no way to send mail")
		return
	case f != nil:
		writeITIP(w, f.status, f.code, f.msg)
		return
	case slices.Contains(out.own, addr):
		writeITIP(w, http.StatusConflict, "own_address", "your own addresses are always allowed")
		return
	}
	path := s.userSettingsPath(u.ID)
	// A dry run first, so a refusal does not spend the cooldown.
	current, err := config.LoadUserSettings(path)
	if err == nil {
		err = change(&current, addr, "", 0)
	}
	if !s.kypulseListError(w, u.ID, action, err) {
		return
	}
	if allowed, retryAfter := s.kypulseConfirmCooldown.allow(u.ID + "|" + addr); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeITIP(w, http.StatusTooManyRequests, "rate_limited", "a confirmation was sent to this address recently; try again later")
		return
	}
	nonce := rand.Text()
	if !s.kypulseListError(w, u.ID, action, config.UpdateUserSettings(path, func(st *config.UserSettings) error {
		return change(st, addr, nonce, time.Now().Unix())
	})) {
		return
	}
	token, _, err := s.createPairingToken(kypulseTokenSubject(u.ID, nonce, addr), pairingPurposeKyPulse, kypulseConfirmTTL)
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "retry later")
		return
	}
	link := s.pickupBaseURL() + "/kypulse/confirm?t=" + token
	text := "KyPulse, a monitoring service, may send alerts from " + out.from + " to this address.\n\n" +
		"To allow it, open the link below and press Confirm:\n\n" + link + "\n\n" +
		"The link expires in 7 days. If you did not expect this, ignore this message: " +
		"no alert is sent to this address unless it is confirmed.\n"
	if f := s.sendKyPulseMail(r.Context(), u, out, "kypulse-confirm:"+u.ID+":"+nonce, []string{addr}, kypulseConfirmSubject, text); f != nil {
		s.logger.Info("kypulse recipient changed", "actor", u.ID, "action", action, "result", "mail_"+f.code)
		if f.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(f.retryAfter))
		}
		writeITIP(w, f.status, f.code, "the address is listed, but the confirmation mail was not sent: "+f.msg)
		return
	}
	s.logger.Info("kypulse recipient changed", "actor", u.ID, "action", action, "result", "ok")
	s.writeKyPulseView(w, r, u.ID)
}

// kypulseTokenSubject binds a confirmation token to the owner, the entry's
// nonce and a hash of the address (the token is readable, so no address).
func kypulseTokenSubject(userID, nonce, addr string) string {
	sum := sha256.Sum256([]byte(addr))
	return userID + "|" + nonce + "|" + hex.EncodeToString(sum[:16])
}

const kypulseConfirmPage = `<!doctype html><html><head><meta charset="utf-8">` +
	`<title>KyPulse alert mail</title><meta name="robots" content="noindex,nofollow"></head>` +
	`<body style="font-family:sans-serif;max-width:640px;margin:40px auto;padding:0 16px">` +
	`<h1>KyPulse alert mail</h1>{{if .Token}}` +
	`<p>Confirm to let KyPulse alerts from a KyPost mailbox be sent to this address.</p>` +
	`<form method="post" action="/kypulse/confirm?t={{.Token}}">` +
	`<button type="submit" style="font-size:1rem;padding:10px 18px;cursor:pointer">Confirm</button></form>` +
	`<p style="color:#666">If you did not expect this, close this page: nothing is sent to this address unless you confirm.</p>` +
	`{{else}}<p>{{.Message}}</p>{{end}}</body></html>`

var kypulseConfirmTemplate = template.Must(template.New("kypulseConfirm").Parse(kypulseConfirmPage))

func (s *Server) renderKyPulseConfirm(w http.ResponseWriter, status int, token, message string) {
	var buf bytes.Buffer
	if err := kypulseConfirmTemplate.Execute(&buf, struct{ Token, Message string }{token, message}); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

const kypulseLinkDead = "This link is invalid, has expired or was already used."

// kypulseConfirmToken is the link's token, "" when links are off.
func (s *Server) kypulseConfirmToken(r *http.Request) string {
	if !s.pairingSecretConfigured() {
		return ""
	}
	return strings.TrimSpace(r.URL.Query().Get("t"))
}

// kypulseSubject splits a verified token's subject: owner, nonce, address hash.
func kypulseSubject(claims pairingTokenClaims, err error) ([]string, bool) {
	parts := strings.Split(claims.Sub, "|")
	return parts, err == nil && len(parts) == 3 && parts[0] != "" && parts[1] != ""
}

// handleKyPulseConfirmPage shows the Confirm button. It changes nothing, so
// a mail scanner fetching the link confirms nothing.
func (s *Server) handleKyPulseConfirmPage(w http.ResponseWriter, r *http.Request) {
	token := s.kypulseConfirmToken(r)
	if _, ok := kypulseSubject(s.decodeAndVerifyPairingToken(token, pairingPurposeKyPulse, time.Now())); token == "" || !ok {
		s.renderKyPulseConfirm(w, http.StatusForbidden, "", kypulseLinkDead)
		return
	}
	s.renderKyPulseConfirm(w, http.StatusOK, token, "")
}

// handleKyPulseConfirm confirms the address the token names, if its entry
// still carries the token's nonce: a resend or removal kills older links,
// and confirming clears the nonce, so a link works once.
func (s *Server) handleKyPulseConfirm(w http.ResponseWriter, r *http.Request) {
	token := s.kypulseConfirmToken(r)
	sub, ok := kypulseSubject(s.decodeAndVerifyPairingToken(token, pairingPurposeKyPulse, time.Now()))
	if token == "" || !ok {
		s.renderKyPulseConfirm(w, http.StatusForbidden, "", kypulseLinkDead)
		return
	}
	want := strings.Join(sub, "|")
	u, err := s.users.Get(sub[0])
	if err != nil {
		s.renderKyPulseConfirm(w, http.StatusGone, "", kypulseLinkDead)
		return
	}
	userID := u.ID // the stored account's ID, not the link's text, names the settings path
	err = config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error {
		for i, x := range st.KyPulse.Recipients {
			if !x.Confirmed && x.Nonce != "" && subtle.ConstantTimeCompare([]byte(kypulseTokenSubject(userID, x.Nonce, x.Address)), []byte(want)) == 1 {
				st.KyPulse.Recipients[i].Confirmed, st.KyPulse.Recipients[i].Nonce = true, ""
				return nil
			}
		}
		return errKyPulseMissing
	})
	switch {
	case errors.Is(err, errKyPulseMissing):
		s.logger.Info("kypulse recipient confirmed", "actor", userID, "result", "dead_link")
		s.renderKyPulseConfirm(w, http.StatusGone, "", kypulseLinkDead)
	case err != nil:
		s.renderKyPulseConfirm(w, http.StatusServiceUnavailable, "", "This could not be saved. Try again later.")
	default:
		s.logger.Info("kypulse recipient confirmed", "actor", userID, "result", "ok")
		s.renderKyPulseConfirm(w, http.StatusOK, "", "Confirmed. KyPulse alerts may now be sent to this address.")
	}
}
