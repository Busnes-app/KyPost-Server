package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/ky-primitives/syncauth"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/kycalendar"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Flagged mail makes KyCalendar tasks (docs/KYCALENDAR.md, Tasks).
const (
	flagTaskPath    = "/api/tasks/flag"
	maxFlagSubject  = 255
	flagTaskTimeout = 5 * time.Second
)

// flagTaskUser is the user whose flags KyCalendar follows: paired, their KyCalendar switch on
// and a live SSO link. nil when task sync does not apply.
func (s *Server) flagTaskUser(userID string) *users.User {
	if !s.kycal.Available() || s.serverBaseURL == "" {
		return nil
	}
	u, err := s.users.Get(userID)
	if err != nil || u.SSOSub == "" || u.SSOIssuer == "" || u.SSOLinkRevokedAt != 0 {
		return nil
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil || !settings.KyCalendarOn(s.kycal.DefaultOn()) {
		return nil
	}
	return &u
}

// flagSubject reads a message's Subject for its task: decoded, one line, at most 255 bytes.
func flagSubject(ctx context.Context, c imapadapter.Client, folder string, uid int) string {
	lines, err := c.FetchHeaderFields(ctx, folder, []int{uid}, "Subject")
	if err != nil {
		return ""
	}
	raw := strings.Join(imapadapter.HeaderMap(lines[uid])["Subject"], " ")
	if dec, err := new(mime.WordDecoder).DecodeHeader(raw); err == nil {
		raw = dec
	}
	raw = strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	for len(raw) > maxFlagSubject {
		_, n := utf8.DecodeLastRuneInString(raw)
		raw = raw[:len(raw)-n]
	}
	return raw
}

// flagTask records one message flagged or unflagged in KyPost and tells KyCalendar, which makes,
// updates or completes its task. It answers the outcome the client shows: KyCalendar's
// (created, updated, completed, unchanged), "none" when no sync applies, or "failed".
// A failure never undoes the flag.
func (s *Server) flagTask(ctx context.Context, userID string, u *users.User, c imapadapter.Client, mailbox, folder, message string, uid int, flagged bool, due string) string {
	flags := kycalendar.OpenFlags(s.userStateDir(userID))
	var ref, subject string
	var err error
	if flagged {
		ref, err = flags.Put(kycalendar.Flag{Mailbox: mailbox, Folder: folder, Message: message, Due: due})
	} else {
		ref, err = flags.Clear(mailbox, folder, message)
	}
	if err != nil {
		return "failed"
	}
	if ref == "" {
		return "none"
	}
	// Local marks follow the mailbox even when task sync is unavailable or exhausted.
	if u == nil {
		return "none"
	}
	if ctx.Err() != nil || uid <= 0 {
		return "failed"
	}
	if flagged {
		subject = flagSubject(ctx, c, folder, uid)
	}
	body := map[string]any{"sub": u.SSOSub, "issuer": u.SSOIssuer, "ref": ref, "subject": subject,
		"url": strings.TrimRight(s.serverBaseURL, "/") + "/read?flag=" + ref, "flagged": flagged}
	if due != "" && flagged {
		body["due"] = due
	}
	rid, err := fsutil.NewUUIDv4()
	if err != nil {
		return "failed"
	}
	eventID := "kypost-flag-" + rid
	status, answer, err := s.kycal.Post(ctx, flagTaskPath, kycalendar.EventTaskFlag, eventID, body)
	outcome := kycalendar.Outcome(answer)
	switch {
	case err != nil:
		outcome = "failed"
	case status != http.StatusOK || (outcome != "created" && outcome != "updated" && outcome != "completed" && outcome != "unchanged" && outcome != "duplicate"):
		outcome = "failed"
	}
	s.logger.Info("kycalendar task flag answered", "event_id", eventID, "result", strconv.Itoa(status)+" "+outcome)
	return outcome
}

// handleListFlags lists the selected mailbox's locally flagged messages, for the
// web's flag marks.
func (s *Server) handleListFlags(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	list, err := kycalendar.OpenFlags(s.userStateDir(ac.UserID)).List()
	if err != nil {
		http.Error(w, "flags unavailable", http.StatusServiceUnavailable)
		return
	}
	out := []map[string]string{}
	for _, f := range list {
		if !f.Cleared && f.Mailbox == ac.Mailbox {
			out = append(out, map[string]string{"ref": f.Ref, "folder": f.Folder, "messageId": f.Message, "due": f.Due})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	// tasks: a flag here also makes a KyCalendar task, so the Flag dialog says so.
	writeJSON(w, http.StatusOK, map[string]any{"flags": out, "tasks": s.flagTaskUser(ac.UserID) != nil})
}

// handleFlagRef resolves a KyCalendar task's link (/read?flag=<ref>) to the user's own message:
// 404 when the reference is unknown or the message is gone, so the web says so.
func (s *Server) handleFlagRef(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	f, found, err := kycalendar.OpenFlags(s.userStateDir(ac.UserID)).Get(r.PathValue("ref"))
	if err != nil {
		http.Error(w, "flags unavailable", http.StatusServiceUnavailable)
		return
	}
	gone := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "This message is no longer available", "code": "gone"})
	}
	if !found {
		gone()
		return
	}
	// The mailbox is re-admitted: access to an extra mailbox may have ended since the flag.
	if _, ok := s.admitMailbox(w, r, f.Mailbox); !ok {
		return
	}
	c, err := s.mailboxMailClient(ac.UserID, f.Mailbox)
	if err != nil {
		http.Error(w, "mailbox unavailable", http.StatusServiceUnavailable)
		return
	}
	// This is pure generation/ID validation; it performs no mailbox or network lookup.
	id, err := imapadapter.ResolveMessageReference(c, f.Message)
	uid, convErr := strconv.Atoi(id)
	if err != nil || convErr != nil {
		gone()
		return
	}
	lines, err := c.FetchHeaderFields(r.Context(), f.Folder, []int{uid}, "Subject")
	if err != nil {
		http.Error(w, "mailbox unavailable", http.StatusBadGateway)
		return
	}
	if _, ok := lines[uid]; !ok {
		gone()
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"mailbox": f.Mailbox, "folder": f.Folder, "messageId": f.Message})
}

// handleTaskState sets or clears a message's flag when KyCalendar's task for it is reopened,
// completed or deleted. Signature only; it never calls KyCalendar back.
func (s *Server) handleTaskState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.kycal.Available() {
		writeITIP(w, http.StatusNotFound, "kycalendar_off", "KyCalendar is not configured on this server")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<12+1))
	if err != nil || len(body) > 1<<12 {
		writeITIP(w, http.StatusBadRequest, "bad_body", "request body unreadable or too large")
		return
	}
	ev, err := s.kycal.Verify(r, kycalendar.EventTaskState, body)
	if errors.Is(err, syncauth.ErrReplay) {
		writeJSON(w, http.StatusOK, map[string]any{"outcome": "duplicate"})
		return
	}
	if errors.Is(err, syncauth.ErrReplayGuardFull) {
		// The replay record could not decide: not a bad signature, retry later.
		w.Header().Set("Retry-After", "5")
		writeITIP(w, http.StatusServiceUnavailable, "unavailable", "KyPost cannot check this request right now; retry")
		return
	}
	if err != nil {
		s.logger.Info("kycalendar request refused", "reason", err.Error())
		writeITIP(w, http.StatusUnauthorized, "bad_signature", "Bad signature")
		return
	}
	outcome, f := s.taskState(r.Context(), body)
	if f != nil {
		s.kycal.Forget(ev.ID)
		s.logger.Info("kycalendar task state refused", "event_id", ev.ID, "result", f.code)
		writeITIP(w, f.status, f.code, f.msg)
		return
	}
	s.logger.Info("kycalendar task state applied", "event_id", ev.ID, "result", outcome)
	writeJSON(w, http.StatusOK, map[string]any{"outcome": outcome})
}

func (s *Server) taskState(ctx context.Context, body []byte) (string, *itipRefusal) {
	var req struct {
		Sub    string `json:"sub"`
		Issuer string `json:"issuer"`
		Ref    string `json:"ref"`
		Done   *bool  `json:"done"`
	}
	if json.Unmarshal(body, &req) != nil || req.Sub == "" || req.Issuer == "" || req.Ref == "" || req.Done == nil {
		return "", refuse(http.StatusBadRequest, "bad_body", "malformed request or missing fields")
	}
	u, err := s.users.GetBySSOSubIssuer(req.Issuer, req.Sub)
	switch {
	case errors.Is(err, users.ErrNotFound) || errors.Is(err, users.ErrNativeAccountConflict):
		return "", refuse(http.StatusNotFound, "no_user", "no active KyPost account for this subject")
	case err != nil:
		return "", errUnavailable
	case !u.Active || u.SSOLinkRevokedAt != 0 || s.nativeMail && u.Role == users.RoleAdmin:
		return "", refuse(http.StatusNotFound, "no_user", "no active KyPost account for this subject")
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		return "", errUnavailable
	}
	if !settings.KyCalendarOn(s.kycal.DefaultOn()) {
		return "", refuse(http.StatusConflict, "sending_off", "the user has not turned KyCalendar on")
	}
	flags := kycalendar.OpenFlags(s.userStateDir(u.ID))
	f, ok, err := flags.Get(req.Ref)
	if err != nil {
		return "", errUnavailable
	}
	if !ok {
		return "gone", nil
	}
	if f.Cleared == *req.Done {
		return "unchanged", nil
	}
	// Access to an extra mailbox may have ended since the flag: then the message is gone for us.
	if f.Mailbox != "" && f.Mailbox != u.ID {
		if _, _, err := s.nativeMailboxAssignment(ctx, u.ID, f.Mailbox); err != nil {
			if errors.Is(err, sso.ErrNativeMailboxUnknown) {
				return "gone", nil
			}
			return "", errUnavailable
		}
	}
	c, err := s.mailboxMailClient(u.ID, f.Mailbox)
	if err != nil {
		return "", errUnavailable
	}
	// This is pure generation/ID validation; it performs no mailbox or network lookup.
	id, err := imapadapter.ResolveMessageReference(c, f.Message)
	if err != nil {
		return "gone", nil
	}
	action, outcome := "flag", "flagged"
	if *req.Done {
		action, outcome = "unflag", "cleared"
	}
	if err := c.ApplyInboxAction(ctx, id, action, f.Folder, ""); err != nil {
		return "", errUnavailable
	}
	if _, err := flags.SetCleared(req.Ref, *req.Done); err != nil {
		return "", errUnavailable
	}
	return outcome, nil
}
