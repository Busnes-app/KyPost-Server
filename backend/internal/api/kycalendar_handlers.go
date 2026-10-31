package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// maxITIPRequestBytes bounds a send request: a 1 MiB object, JSON-escaped.
const maxITIPRequestBytes = 4 << 20

// SetKyCalendar pairs KyCalendar (docs/KYCALENDAR.md). Startup-only; nil or
// an unpaired Client leaves the feature off.
func (s *Server) SetKyCalendar(c *kycalendar.Client) { s.kycal = c }

// itipRefusal is a contract answer: KyCalendar records code.
type itipRefusal struct {
	status     int
	code, msg  string
	retryAfter int
	// keep leaves the event ID in the replay guard: something may have gone
	// out, so a retry must not send again.
	keep bool
}

func writeITIP(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// handleITIPSend mails an iTIP object for KyCalendar from the user's own
// mailbox. Signature only; every check below is a refusal KyCalendar records.
func (s *Server) handleITIPSend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.kycal.Available() {
		writeITIP(w, http.StatusNotFound, "kycalendar_off", "KyCalendar is not configured on this server")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxITIPRequestBytes+1))
	if err != nil || len(body) > maxITIPRequestBytes {
		writeITIP(w, http.StatusBadRequest, "bad_body", "request body unreadable or too large")
		return
	}
	ev, err := s.kycal.Verify(r, kycalendar.EventSend, body)
	if errors.Is(err, syncauth.ErrReplay) {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "duplicate"})
		return
	}
	if err != nil {
		s.logger.Info("kycalendar request refused", "reason", err.Error())
		writeITIP(w, http.StatusUnauthorized, "bad_signature", "Bad signature")
		return
	}
	if f := s.sendITIP(r.Context(), ev.ID, body); f != nil {
		if !f.keep {
			s.kycal.Forget(ev.ID)
		}
		if f.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(f.retryAfter))
		}
		s.logger.Info("kycalendar send refused", "event_id", ev.ID, "result", f.code)
		writeITIP(w, f.status, f.code, f.msg)
		return
	}
	s.logger.Info("kycalendar send completed", "event_id", ev.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"outcome": "sent"})
}

func refuse(status int, code, msg string) *itipRefusal {
	return &itipRefusal{status: status, code: code, msg: msg}
}

var errUnavailable = refuse(http.StatusServiceUnavailable, "unavailable", "storage unavailable; retry later")

func (s *Server) sendITIP(ctx context.Context, eventID string, body []byte) *itipRefusal {
	var req struct {
		Sub        string   `json:"sub"`
		Issuer     string   `json:"issuer"`
		From       string   `json:"from"`
		Recipients []string `json:"recipients"`
		Method     string   `json:"method"`
		ICS        string   `json:"ics"`
	}
	if json.Unmarshal(body, &req) != nil || req.Sub == "" || req.Issuer == "" || req.From == "" || req.ICS == "" ||
		len(req.Recipients) == 0 || len(req.Recipients) > maxRecipientsPerSend {
		return refuse(http.StatusBadRequest, "bad_body", "malformed request or missing fields")
	}
	if req.Method != "REQUEST" && req.Method != "CANCEL" && req.Method != "REPLY" {
		return refuse(http.StatusBadRequest, "bad_body", "method must be REQUEST, CANCEL or REPLY")
	}
	var recipients []string
	for _, raw := range req.Recipients {
		a, err := mail.ParseAddress(raw)
		if err != nil || a.Name != "" || a.Address != strings.TrimSpace(raw) {
			return refuse(http.StatusBadRequest, "bad_body", "recipients must be bare addresses")
		}
		if addr := strings.ToLower(a.Address); !slices.Contains(recipients, addr) {
			recipients = append(recipients, addr)
		}
	}

	u, err := s.users.GetBySSOSubIssuer(req.Issuer, req.Sub)
	switch {
	case errors.Is(err, users.ErrNotFound) || errors.Is(err, users.ErrNativeAccountConflict):
		return refuse(http.StatusNotFound, "no_user", "no active KyPost account for this subject")
	case err != nil:
		return errUnavailable
	case !u.Active || u.SSOLinkRevokedAt != 0:
		return refuse(http.StatusNotFound, "no_user", "no active KyPost account for this subject")
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		return errUnavailable
	}
	if !settings.KyCalendarOn(s.kycal.DefaultOn()) {
		return refuse(http.StatusConflict, "sending_off", "the user has not let KyCalendar send from their mailbox")
	}
	if u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
		return refuse(http.StatusConflict, "native_unsupported", "KyCalendar sending is not available for KyPost-hosted mailboxes yet")
	}
	payload, exists, err := s.outboundMailConfig(u.ID)
	if err != nil {
		return errUnavailable
	}
	smtpHost, smtpPort, addr, err := mailmsg.ResolveSMTPTarget(payload)
	accountAddr := sanitizeHeaderValue(payload.Username)
	if !exists || err != nil || accountAddr == "" {
		return refuse(http.StatusConflict, "no_mailbox", "the user has no outgoing mail server configured")
	}
	headerFrom, envelopeFrom, status, _ := resolveMailFrom(accountAddr, req.From, func() (*sendas.Store, error) {
		return s.userSendAsStore(u.ID)
	})
	if status == http.StatusInternalServerError {
		return errUnavailable
	}
	if status != 0 {
		return refuse(http.StatusConflict, "bad_from", "from is neither the account address nor a verified send-as alias")
	}

	it, ics, err := mailmsg.ParseITIP(req.ICS, req.Method)
	if err != nil {
		return refuse(http.StatusUnprocessableEntity, "bad_ics", err.Error())
	}
	sender := strings.ToLower(envelopeFrom)
	allowed := it.Attendees
	if req.Method == "REPLY" {
		if !slices.Contains(it.Attendees, sender) {
			return refuse(http.StatusConflict, "bad_recipient", "from must be an ATTENDEE of the reply")
		}
		allowed = []string{it.Organizer}
	} else if it.Organizer != sender {
		return refuse(http.StatusConflict, "bad_recipient", "from must be the ORGANIZER")
	}
	for _, rcpt := range recipients {
		if !slices.Contains(allowed, rcpt) {
			return refuse(http.StatusConflict, "bad_recipient", "every recipient must be named by the object")
		}
	}
	if req.Method == "REPLY" {
		seen, err := kycalendar.OpenInvitations(s.userStateDir(u.ID)).Has(it.UID, it.Organizer)
		if err != nil {
			return errUnavailable
		}
		if !seen {
			return refuse(http.StatusConflict, "no_invitation", "this mailbox holds no verified invitation for that event from that organizer")
		}
	}

	// Keyed by the event ID, so a retry of a lost answer is charged once.
	wait, err := s.chargeSend(ctx, "kycalendar:"+u.ID+":"+eventID, u.ID, len(recipients), time.Now())
	switch {
	case errors.Is(err, state.ErrSendQuota):
		f := refuse(http.StatusTooManyRequests, "rate_limited", "the user's sending quota is reached")
		f.retryAfter = max(1, int(wait.Seconds()))
		return f
	case errors.Is(err, state.ErrSendTooLarge):
		return refuse(http.StatusUnprocessableEntity, "too_many_recipients", "more recipients than the user's daily quota")
	case err != nil:
		return errUnavailable
	}

	msg, sent, _ := composeSend(mailmsg.Message{
		From:           headerFrom,
		To:             recipients,
		Subject:        mailmsg.ITIPSubject(it),
		Body:           itipBody[req.Method],
		Calendar:       ics,
		CalendarMethod: req.Method,
	}, nil)
	if err := mailmsg.SMTPDeliver(smtpHost, smtpPort, addr, payload.Username, payload.Password, envelopeFrom, recipients, msg); err != nil &&
		!errors.Is(err, mailmsg.ErrSMTPAcceptedThenFailed) {
		f := refuse(http.StatusServiceUnavailable, "smtp_failed", "the user's mail server did not accept the message; retry later")
		f.keep = errors.Is(err, mailmsg.ErrSMTPAcceptanceUncertain)
		return f
	}
	if mailClient, err := s.userMailClient(u.ID); err == nil {
		if err := mailClient.SaveSent(ctx, imapadapter.DraftMessage{To: recipients, Raw: sent}); err != nil {
			s.logger.Error("kycalendar send not saved to Sent", "event_id", eventID)
		}
	}
	return nil
}

var itipBody = map[string]string{
	"REQUEST": "You are invited to an event. Your calendar app can add it and answer from the attached invitation.\r\n",
	"CANCEL":  "This event has been cancelled. Your calendar app can remove it using the attached update.\r\n",
	"REPLY":   "This is an answer to your invitation. Your calendar app records it from the attached reply.\r\n",
}

// handleAdminKyCalendar reports how KyCalendar is paired: through KyIdentity,
// by the environment, or not, and why. Never the secret.
func (s *Server) handleAdminKyCalendar(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.kycal.Status())
}

// handleKyCalendarSettings reads and sets the caller's KyCalendar switch.
// available reports whether this server is paired with KyCalendar at all.
func (s *Server) handleKyCalendarSettings(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	path := s.userSettingsPath(ac.UserID)
	if r.Method == http.MethodPut {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req) != nil || req.Enabled == nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := config.UpdateUserSettings(path, func(st *config.UserSettings) error {
			st.KyCalendar = req.Enabled
			return nil
		}); err != nil {
			http.Error(w, "failed to save settings", http.StatusInternalServerError)
			return
		}
	}
	settings, err := config.LoadUserSettings(path)
	if err != nil {
		http.Error(w, "failed to read settings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": s.kycal.Available(), "enabled": settings.KyCalendarOn(s.kycal.DefaultOn())})
}

// handleCalendarRSVP answers an invitation in the caller's mailbox through
// KyCalendar. The invitation is read from the message server-side; a client
// never supplies the ics. Off (409 kycalendar_off) when the server is not
// paired or the user has not turned KyCalendar on: the client then sends its
// calendarReply through POST /api/mail/send as before.
func (s *Server) handleCalendarRSVP(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var req struct {
		MessageID    string `json:"messageId"`
		Mailbox      string `json:"mailbox"`
		Index        int    `json:"index"`
		PartStat     string `json:"partstat"`
		RecurrenceID string `json:"recurrenceId"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req) != nil || req.Index < 0 || len(req.RecurrenceID) > 64 ||
		strings.ContainsFunc(req.RecurrenceID, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		writeITIP(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if req.PartStat != "ACCEPTED" && req.PartStat != "TENTATIVE" && req.PartStat != "DECLINED" {
		writeITIP(w, http.StatusBadRequest, "bad_partstat", "partstat must be ACCEPTED, TENTATIVE or DECLINED")
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "account unavailable")
		return
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "settings unavailable")
		return
	}
	if !s.kycal.Available() || !settings.KyCalendarOn(s.kycal.DefaultOn()) || u.SSOSub == "" || u.SSOIssuer == "" || u.SSOLinkRevokedAt != 0 {
		writeITIP(w, http.StatusConflict, "kycalendar_off", "KyCalendar is not in use for this account; send the reply by mail")
		return
	}
	mailClient, err := s.mailFor(r)
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "mailbox_unavailable", "could not open the mailbox")
		return
	}
	if req.Mailbox != "" && imapadapter.ValidateMailboxName(req.Mailbox) != nil {
		writeITIP(w, http.StatusBadRequest, "bad_request", "invalid mailbox")
		return
	}
	id, err := imapadapter.ResolveMessageReference(mailClient, strings.TrimSpace(req.MessageID))
	uid, convErr := strconv.Atoi(id)
	if err != nil || convErr != nil || uid <= 0 {
		writeITIP(w, http.StatusBadRequest, "bad_request", "invalid messageId; refresh the mailbox")
		return
	}
	info, content, err := mailClient.GetAttachment(r.Context(), req.Mailbox, uid, req.Index)
	if errors.Is(err, imapadapter.ErrAttachmentNotFound) {
		writeITIP(w, http.StatusNotFound, "not_found", "invitation not found")
		return
	}
	if err != nil {
		writeITIP(w, http.StatusBadGateway, "mailbox_unavailable", "could not read the invitation")
		return
	}
	it, ics, err := mailmsg.ParseITIP(string(content), "REQUEST")
	if !strings.EqualFold(strings.TrimSpace(strings.Split(info.MimeType, ";")[0]), "text/calendar") || err != nil {
		writeITIP(w, http.StatusUnprocessableEntity, "not_invitation", "that attachment is not a calendar invitation")
		return
	}
	body := map[string]any{"sub": u.SSOSub, "issuer": u.SSOIssuer, "uid": it.UID, "partstat": req.PartStat, "ics": string(ics)}
	if req.RecurrenceID != "" {
		body["recurrence_id"] = req.RecurrenceID
	}
	// One answer, one event: KyPost does not retry it, the user does.
	rid, err := fsutil.NewUUIDv4()
	eventID := "kypost-respond-" + rid
	if err != nil {
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "retry later")
		return
	}
	status, answer, err := s.kycal.Post(r.Context(), "/api/itip/respond", kycalendar.EventRespond, eventID, body)
	if err != nil {
		s.logger.Error("kycalendar respond failed", "event_id", eventID, "reason", "network")
		writeITIP(w, http.StatusBadGateway, "kycalendar_unavailable", "KyCalendar could not be reached; try again")
		return
	}
	code := kycalendar.Outcome(answer)
	s.logger.Info("kycalendar respond answered", "event_id", eventID, "result", strconv.Itoa(status)+" "+code)
	switch {
	case status == http.StatusOK:
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "answered"})
	case rsvpErrors[code] != "" && (status < 500 || status == http.StatusInsufficientStorage):
		writeITIP(w, status, code, rsvpErrors[code])
	default:
		writeITIP(w, http.StatusBadGateway, "kycalendar_unavailable", "KyCalendar could not record the answer; try again")
	}
}

// rsvpErrors are the respond codes a client can act on.
var rsvpErrors = map[string]string{
	"no_event":     "KyCalendar has no event for this invitation",
	"not_invited":  "this account is not invited to that event",
	"bad_partstat": "KyCalendar refused that answer",
	"conflict":     "the event changed; reload the invitation and answer again",
	"quota":        "the calendar is full",
}
