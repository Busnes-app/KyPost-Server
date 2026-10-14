// Scheduled send: a message built and frozen now, sent later by the sweeper.
// The frozen job carries the sender's authority snapshot; if any of it changed
// by send time the message is not sent and goes back to Drafts with a reason.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const (
	maxScheduledPending    = 50
	scheduledMinLead       = time.Minute
	scheduledMaxLead       = 366 * 24 * time.Hour
	scheduledRetryWindow   = time.Hour
	scheduledRetryDelay    = 5 * time.Minute
	scheduledSweepInterval = 30 * time.Second
	scheduledRetention     = 30 * 24 * time.Hour
	// One user's pending sealed total, so scheduled mail cannot become
	// unbounded storage outside every quota.
	scheduledMaxUserBytes = 256 << 20
	// Rows are worked by a small pool, at most one per user at a time, each
	// under its own deadline, so one account's unreachable server cannot hold
	// up everyone else's mail.
	scheduledWorkers    = 4
	scheduledRowTimeout = 3 * time.Minute
	// A draft that cannot be saved for this long is given up as failed.
	scheduledReturnFor = 7 * 24 * time.Hour
)

// scheduledMaxRowBytes caps one sealed job at the native outbox's job limit.
// A var only so a test can lower it.
var scheduledMaxRowBytes = 128 << 20

// Reasons shown with a message that was not sent.
const (
	reasonCanceled = "canceled"
	reasonAccount  = "account security changed (password, two-factor, role or sign-in); review it and send again"
	reasonKeys     = "your PGP keys changed; review it and send again"
	reasonDevice   = "the device that scheduled it was removed or paired again"
	reasonFrom     = "the From address is no longer one you may send as"
	reasonSender   = "sending authority for this address changed (address, domain or relay); review it and send again"
	reasonMail     = "the mail account settings changed"
	reasonFull     = "the mailbox is full"
	reasonLate     = "it could not be sent within an hour of its send time"
	reasonLost     = "the scheduled message could not be opened; nothing was sent"
	reasonGone     = "the account was removed; nothing was sent"
	reasonNoDraft  = "it could not be saved to Drafts for a week; nothing was sent"
)

// scheduledJob is the frozen send, sealed at rest. Job holds the deliveries,
// the native Sent copy and the authority snapshot rechecked at send time.
type scheduledJob struct {
	ID, UserID, Mailbox string
	Native              bool
	Job                 mailbox.OutboundJob
	// IMAP only: the account and requested From it was authorized under.
	Username, RequestedFrom string
	Sent                    *imapadapter.DraftMessage
	Draft                   imapadapter.DraftMessage
}

// scheduledSummary is what the owner sees in the list: no more than the
// composer already showed them.
type scheduledSummary struct {
	Subject string   `json:"subject"`
	To      []string `json:"to"`
}

// parseSendAt reads an optional RFC 3339 sendAt; empty means send now.
func parseSendAt(raw string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, errors.New("sendAt must be an RFC 3339 time")
	}
	if at.Before(now.Add(scheduledMinLead)) || at.After(now.Add(scheduledMaxLead)) {
		return time.Time{}, errors.New("sendAt must be between one minute and 366 days from now")
	}
	return at, nil
}

func (s *Server) scheduledSendKey() ([]byte, error) {
	return cryptutil.LoadOrCreateKey(filepath.Join(config.SecretDir(), "scheduled-send.key"))
}

func sealScheduled(key []byte, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	env, err := cryptutil.Seal(raw, key)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func openScheduled(key, blob []byte, v any) error {
	var env cryptutil.EncryptedPayload
	if err := json.Unmarshal(blob, &env); err != nil {
		return err
	}
	raw, err := cryptutil.Open(env, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// openScheduledRow opens a row's job and refuses one sealed for another row,
// so a blob copied between rows or users cannot be sent as theirs.
func openScheduledRow(key []byte, r state.ScheduledSend) (scheduledJob, error) {
	var j scheduledJob
	if err := openScheduled(key, r.Sealed, &j); err != nil {
		return j, err
	}
	if j.ID != r.ID || j.UserID != r.UserID || j.Mailbox != r.Mailbox {
		return j, errors.New("scheduled job does not belong to its row")
	}
	return j, nil
}

// scheduledDraft is what returns to Drafts if the send cannot go out. An
// account whose drafts are encrypted, or an encrypted send, never gets a
// plaintext one: the browser's pgpDraft, else the self-encrypted Sent copy.
func scheduledDraft(w http.ResponseWriter, req mailRequest, u users.User, encryptedCopy []byte) (imapadapter.DraftMessage, bool) {
	if req.PGPDraft != "" {
		if err := validatePGPMimeDeliveryShape(req.PGPDraft); err != nil {
			http.Error(w, "encrypted draft: "+err.Error(), http.StatusBadRequest)
			return imapadapter.DraftMessage{}, false
		}
		return imapadapter.DraftMessage{To: req.To, Subject: pgpmail.OuterPlaceholderSubject, Raw: []byte(req.PGPDraft)}, true
	}
	if u.PGPProtection() == users.PGPProtectionClient {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "scheduling needs an encrypted copy of the draft; reload this client", "clientSideNeeded": true})
		return imapadapter.DraftMessage{}, false
	}
	if req.Encrypt {
		if len(encryptedCopy) == 0 {
			http.Error(w, "scheduling an encrypted message needs a PGP key of your own, so it can return to Drafts encrypted", http.StatusConflict)
			return imapadapter.DraftMessage{}, false
		}
		return imapadapter.DraftMessage{To: req.To, Subject: pgpmail.OuterPlaceholderSubject, Raw: encryptedCopy}, true
	}
	return imapadapter.DraftMessage{To: req.To, CC: req.CC, BCC: req.BCC, Subject: req.Subject, Body: req.Body, Mode: req.Mode, Attachments: req.Attachments}, true
}

// scheduleClientPGP freezes a browser-encrypted send; its draft is the
// browser's draftCopy.
func (s *Server) scheduleClientPGP(w http.ResponseWriter, r *http.Request, ac AuthContext, u users.User, req clientEncryptedSendRequest, native bool, from, username string, deliveries []mailbox.OutboundDelivery, nativeSent []byte, sent *imapadapter.DraftMessage) {
	job, ok := outboundJob(w, ac, u, from, deliveries, nativeSent, true, req.MaterialGeneration, 0)
	if !ok {
		return
	}
	draft := imapadapter.DraftMessage{To: req.To, Subject: pgpmail.OuterPlaceholderSubject, Raw: []byte(strings.TrimSpace(req.DraftCopy))}
	s.scheduleSend(w, r, ac, u, req.sendAt, scheduledJob{Native: native, Job: job, Username: username, RequestedFrom: req.From, Sent: sent, Draft: draft}, pgpmail.OuterPlaceholderSubject, req.To)
}

// scheduleSend seals j and stores it to go out at sendAt.
func (s *Server) scheduleSend(w http.ResponseWriter, r *http.Request, ac AuthContext, u users.User, sendAt time.Time, j scheduledJob, subject string, to []string) {
	if s.globalStore == nil {
		http.Error(w, "scheduled send unavailable; send now or retry later", http.StatusServiceUnavailable)
		return
	}
	key, err := s.scheduledSendKey()
	id, idErr := fsutil.NewUUIDv4()
	if err != nil || idErr != nil {
		http.Error(w, "scheduled send unavailable; send now or retry later", http.StatusServiceUnavailable)
		return
	}
	j.ID, j.UserID, j.Mailbox = id, ac.UserID, cmp.Or(ac.Mailbox, ac.UserID)
	j.Job.PGPFingerprint = u.PGPFingerprint
	if j.Native {
		// Frozen so a released and re-added alias cannot inherit the job.
		addresses, err := s.ssoLifecycle.NativeAddresses()
		if err != nil {
			http.Error(w, "native mailbox unavailable; preserve mail and repair account authority", http.StatusServiceUnavailable)
			return
		}
		j.Job.FromGeneration = addresses[j.Job.From].Generation
	}
	sealed, err := sealScheduled(key, j)
	summary, summaryErr := sealScheduled(key, scheduledSummary{Subject: subject, To: to})
	if err != nil || summaryErr != nil {
		http.Error(w, "scheduled send unavailable; send now or retry later", http.StatusServiceUnavailable)
		return
	}
	if len(sealed) > scheduledMaxRowBytes {
		http.Error(w, "this message is too large to schedule; send it now instead", http.StatusRequestEntityTooLarge)
		return
	}
	if err := fsutil.CheckDriveReserve(s.stateDir, int64(len(sealed))); err != nil {
		http.Error(w, "the server is low on disk space; send now or retry later", http.StatusInsufficientStorage)
		return
	}
	err = s.globalStore.AddScheduledSend(r.Context(), state.ScheduledSend{ID: id, UserID: j.UserID, Mailbox: j.Mailbox, SendAt: sendAt.Unix(), CreatedAt: time.Now().Unix(), Sealed: sealed, Summary: summary}, maxScheduledPending, scheduledMaxUserBytes)
	if errors.Is(err, state.ErrScheduledFull) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "scheduled send unavailable; send now or retry later", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"scheduled": true, "id": id, "sendAt": sendAt.UTC().Format(time.RFC3339)})
}

// handleScheduledList lists the selected mailbox's scheduled mail.
func (s *Server) handleScheduledList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.globalStore == nil {
		http.Error(w, "scheduled send unavailable", http.StatusServiceUnavailable)
		return
	}
	rows, err := s.globalStore.ListScheduledSends(r.Context(), ac.UserID, cmp.Or(ac.Mailbox, ac.UserID))
	key, keyErr := s.scheduledSendKey()
	if err != nil || keyErr != nil {
		http.Error(w, "scheduled send unavailable", http.StatusServiceUnavailable)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var sum scheduledSummary
		_ = openScheduled(key, row.Summary, &sum)
		out = append(out, map[string]any{"id": row.ID, "status": row.Status, "reason": row.Reason, "sendAt": time.Unix(row.SendAt, 0).UTC().Format(time.RFC3339), "subject": sum.Subject, "to": sum.To})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scheduled": out})
}

// handleScheduledCancel returns waiting mail to Drafts. Once the sweeper has
// claimed it, it may already be on the wire, so cancel answers 409.
func (s *Server) handleScheduledCancel(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.globalStore == nil {
		http.Error(w, "scheduled send unavailable", http.StatusServiceUnavailable)
		return
	}
	row, found, err := s.globalStore.GetScheduledSend(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "scheduled send unavailable", http.StatusServiceUnavailable)
		return
	}
	if !found || row.UserID != ac.UserID || row.Mailbox != cmp.Or(ac.Mailbox, ac.UserID) {
		http.Error(w, "scheduled message not found", http.StatusNotFound)
		return
	}
	// The delay keeps the sweeper off this row while it is saved here.
	moved, err := s.globalStore.MoveScheduledSend(r.Context(), row.ID, state.ScheduledWaiting, state.ScheduledReturning, reasonCanceled, time.Now().Add(2*time.Minute).Unix())
	if err != nil {
		http.Error(w, "scheduled send unavailable", http.StatusServiceUnavailable)
		return
	}
	if !moved {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this message is no longer waiting and cannot be canceled", "status": currentScheduledStatus(r.Context(), s.globalStore, row)})
		return
	}
	key, err := s.scheduledSendKey()
	row.Reason = reasonCanceled
	if err != nil || !s.saveScheduledDraft(r.Context(), key, row, time.Now) {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": state.ScheduledReturning, "warning": "canceled; it will be saved to Drafts shortly"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "returned"})
}

func currentScheduledStatus(ctx context.Context, store *state.Store, row state.ScheduledSend) string {
	if now, found, err := store.GetScheduledSend(ctx, row.ID); err == nil && found {
		return now.Status
	}
	return row.Status
}

// StartScheduledSendSweeper sends due scheduled mail. API process only: the
// claim is atomic in SQLite, so a second runner could not double-send, but
// one would mark the other's in-flight rows uncertain at its start.
func (s *Server) StartScheduledSendSweeper(ctx context.Context) {
	if s.globalStore == nil {
		return
	}
	if err := s.globalStore.InterruptScheduledSends(context.WithoutCancel(ctx)); err != nil {
		s.logger.Error("scheduled send recovery failed", "error", err.Error())
	}
	ticker := time.NewTicker(scheduledSweepInterval)
	defer ticker.Stop()
	for {
		s.sweepScheduledSends(ctx, time.Now)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepScheduledSends sends every due row, then saves returning drafts. Times
// are read when each row is worked, never from the sweep's start, so a long
// sweep cannot send past a row's deadline.
func (s *Server) sweepScheduledSends(ctx context.Context, now func() time.Time) {
	// An idle server never touches the key: only scheduling creates it.
	if pending, err := s.globalStore.HasScheduledWork(ctx); err != nil || !pending {
		_ = s.globalStore.PruneScheduledSends(ctx, now().Add(-scheduledRetention).Unix())
		return
	}
	key, err := s.scheduledSendKey()
	if err != nil {
		s.logger.Error("scheduled send key unavailable", "error", err.Error())
		return
	}
	var wg sync.WaitGroup
	for range scheduledWorkers {
		wg.Go(func() {
			for ctx.Err() == nil {
				row, ok, err := s.globalStore.ClaimScheduledSend(ctx, now().Unix())
				if err != nil || !ok {
					return
				}
				// Shutdown stops new claims; a claimed row finishes under its own
				// deadline, so a restart does not leave it uncertain.
				rowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledRowTimeout)
				s.runScheduled(rowCtx, key, row, now)
				cancel()
			}
		})
	}
	wg.Wait()
	returns, err := s.globalStore.ScheduledReturns(ctx, now().Unix())
	if err == nil {
		work := make(chan state.ScheduledSend)
		for range scheduledWorkers {
			wg.Go(func() {
				for row := range work {
					rowCtx, cancel := context.WithTimeout(ctx, scheduledRowTimeout)
					s.saveScheduledDraft(rowCtx, key, row, now)
					cancel()
				}
			})
		}
		for _, row := range returns {
			work <- row
		}
		close(work)
		wg.Wait()
	}
	_ = s.globalStore.PruneScheduledSends(ctx, now().Add(-scheduledRetention).Unix())
}

// runScheduled finishes one claimed row; it never leaves it in sending.
func (s *Server) runScheduled(ctx context.Context, key []byte, row state.ScheduledSend, now func() time.Time) {
	move := func(to, reason string, next int64) {
		if _, err := s.globalStore.MoveScheduledSend(context.WithoutCancel(ctx), row.ID, state.ScheduledSending, to, reason, next); err != nil {
			s.logger.Error("scheduled send state update failed", "correlation_id", row.ID, "error", err.Error())
		}
	}
	j, err := openScheduledRow(key, row)
	if err != nil {
		move("failed", reasonLost, 0)
		return
	}
	deadline := time.Unix(row.SendAt, 0).Add(scheduledRetryWindow)
	// A row held for quota keeps saying so, through to its return.
	late := reasonLate
	if row.Reason == reasonQuota {
		late = reasonQuota
	}
	next, reason := state.ScheduledReturning, late
	if !now().After(deadline) {
		next, reason = s.dispatchScheduled(ctx, j, now)
	}
	if next == state.ScheduledWaiting {
		if retry := now().Add(scheduledRetryDelay); retry.Before(deadline) {
			move(state.ScheduledWaiting, reason, retry.Unix())
			return
		}
		next, reason = state.ScheduledReturning, cmp.Or(reason, reasonLate)
	}
	move(next, reason, 0)
	if next == state.ScheduledReturning {
		row.Reason = reason
		s.saveScheduledDraft(ctx, key, row, now)
	}
}

// dispatchScheduled rechecks authority and sends, naming the row's next
// status: a final one, returning with a reason, or scheduled to retry.
func (s *Server) dispatchScheduled(ctx context.Context, j scheduledJob, now func() time.Time) (string, string) {
	retry := state.ScheduledWaiting
	reason, err := s.scheduledAuthority(ctx, j)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return "failed", reasonGone
	case err != nil:
		return retry, ""
	case reason != "":
		return state.ScheduledReturning, reason
	}
	recipients := 0
	for _, d := range j.Job.Deliveries {
		recipients += len(d.Recipients)
	}
	switch _, err := s.chargeSend(ctx, j.ID, j.UserID, recipients, now()); {
	case errors.Is(err, state.ErrSendQuota):
		s.logger.Warn("sending quota reached", "user_id", j.UserID, "correlation_id", j.ID)
		return retry, reasonQuota
	case errors.Is(err, state.ErrSendTooLarge):
		return state.ScheduledReturning, err.Error()
	case err != nil:
		return retry, ""
	}
	if j.Native {
		// The outbox worker delivers it and files Sent; the job ID is the row's.
		err := s.nativeOutbound().Queue(ctx, j.Mailbox, j.ID, j.Job)
		switch {
		case err == nil:
			return "queued", "handed to the outbox for delivery"
		case errors.Is(err, sso.ErrNativeOutboundStale):
			return state.ScheduledReturning, reasonSender
		case errors.Is(err, state.ErrNativeSendDevice):
			return state.ScheduledReturning, reasonDevice
		case errors.Is(err, mailbox.ErrCapacity):
			return state.ScheduledReturning, reasonFull
		}
		return retry, ""
	}
	account, reason, err := s.scheduledSMTPAccount(j)
	if err != nil {
		return retry, ""
	}
	if reason != "" {
		return state.ScheduledReturning, reason
	}
	groups := make([]smtpGroup, 0, len(j.Job.Deliveries))
	for _, d := range j.Job.Deliveries {
		msg, err := mailmsg.PrepareSMTPMessage(d.Raw)
		if err != nil {
			return state.ScheduledReturning, reasonLost
		}
		groups = append(groups, smtpGroup{recipients: d.Recipients, msg: msg})
	}
	failed, saved, err := s.deliverIMAP(ctx, j.UserID, account, j.Job.From, groups, j.Sent)
	switch {
	case err == nil:
		warning := partialDeliveryWarning(failed, len(groups)-1, 0, 0)
		if j.Sent != nil && !saved {
			warning = joinWarnings(warning, "the Sent copy could not be saved")
		}
		return "sent", warning
	case errors.Is(err, mailmsg.ErrSMTPAcceptedThenFailed):
		return "sent", "the relay accepted it but the session failed; blind copies and the Sent copy were not completed"
	case errors.Is(err, mailmsg.ErrSMTPAcceptanceUncertain):
		return "uncertain", "delivery could not be confirmed; check with your recipients before sending again"
	}
	s.logger.Error("scheduled send attempt failed", "correlation_id", j.ID, "error", err.Error())
	return retry, ""
}

// scheduledAuthority compares the account, keys and sending device with the
// snapshot frozen at schedule time; a reason means do not send.
func (s *Server) scheduledAuthority(ctx context.Context, j scheduledJob) (string, error) {
	u, err := s.users.Get(j.UserID)
	if err != nil {
		return "", err
	}
	if !u.Active || u.MustChangePassword || u.NativeSendEpoch != j.Job.NativeSendEpoch {
		return reasonAccount, nil
	}
	if sso.SenderAuthorityCurrent(u, j.Job) != nil {
		return reasonKeys, nil
	}
	if j.Job.DeviceID == "" {
		return "", nil
	}
	store, err := s.userStore(j.UserID)
	if err != nil {
		return "", err
	}
	err = store.WithNativeSendDevice(ctx, j.Job.DeviceID, func(d state.NativeDevice) error {
		return sso.DeviceAuthorityCurrent(u, j.Job, d)
	})
	if errors.Is(err, state.ErrNativeSendDevice) || errors.Is(err, sso.ErrNativeOutboundStale) {
		return reasonDevice, nil
	}
	return "", err
}

// scheduledSMTPAccount reads the IMAP account's SMTP target, refusing when the
// account or the From it was scheduled under is no longer the same.
func (s *Server) scheduledSMTPAccount(j scheduledJob) (smtpAccount, string, error) {
	payload, exists, err := s.outboundMailConfig(j.UserID)
	if err != nil {
		return smtpAccount{}, "", err
	}
	if !exists || sanitizeHeaderValue(payload.Username) != j.Username {
		return smtpAccount{}, reasonMail, nil
	}
	host, port, addr, err := mailmsg.ResolveSMTPTarget(payload)
	if err != nil {
		return smtpAccount{}, reasonMail, nil
	}
	_, envelopeFrom, status, _ := resolveMailFrom(j.Username, j.RequestedFrom, func() (*sendas.Store, error) {
		return s.userSendAsStore(j.UserID)
	})
	if status == http.StatusInternalServerError {
		return smtpAccount{}, "", errors.New("send-as aliases unavailable")
	}
	if status != 0 || envelopeFrom != j.Job.From {
		return smtpAccount{}, reasonFrom, nil
	}
	return smtpAccount{host: host, port: port, addr: addr, username: payload.Username, password: payload.Password}, "", nil
}

// saveScheduledDraft files a returning row's draft and marks it returned. A
// failed save keeps it returning for a later sweep; it reports success.
func (s *Server) saveScheduledDraft(ctx context.Context, key []byte, row state.ScheduledSend, now func() time.Time) bool {
	move := func(to, reason string, next int64) {
		if _, err := s.globalStore.MoveScheduledSend(context.WithoutCancel(ctx), row.ID, state.ScheduledReturning, to, reason, next); err != nil {
			s.logger.Error("scheduled send state update failed", "correlation_id", row.ID, "error", err.Error())
		}
	}
	j, err := openScheduledRow(key, row)
	if err != nil {
		move("failed", reasonLost, 0)
		return false
	}
	if _, err := s.users.Get(j.UserID); errors.Is(err, users.ErrNotFound) {
		move("failed", reasonGone, 0)
		return false
	}
	// A primary mailbox's ID is its user's; the client API names it "".
	mailboxID := j.Mailbox
	if mailboxID == j.UserID {
		mailboxID = ""
	}
	client, err := s.mailboxMailClient(j.UserID, mailboxID)
	if err == nil {
		err = client.SaveDraft(ctx, j.Draft)
	}
	if err != nil {
		s.logger.Error("scheduled send draft not saved", "correlation_id", row.ID, "error", err.Error())
		if row.ReturningSince > 0 && now().Sub(time.Unix(row.ReturningSince, 0)) > scheduledReturnFor {
			move("failed", reasonNoDraft, 0)
			return false
		}
		move(state.ScheduledReturning, row.Reason, now().Add(scheduledRetryDelay).Unix())
		return false
	}
	move("returned", row.Reason, 0)
	return true
}
