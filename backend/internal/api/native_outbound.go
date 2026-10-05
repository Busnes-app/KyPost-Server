package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func (s *Server) nativeOutbound() sso.NativeOutbound {
	return sso.NativeOutbound{ConfigDir: s.configDir, StateRoot: s.stateDir, SecretDir: config.SecretDir(), Accounts: s.users, Domains: s.nativeDomains, Settings: s.ssoStore, Logger: s.logger}
}

// The existing OK response still means confirmed primary SMTP acceptance. A
// durable queue entry alone cannot tell older clients that mail was sent.
func (s *Server) finishNativeSend(w http.ResponseWriter, r *http.Request, ac AuthContext, u users.User, from string, deliveries []mailbox.OutboundDelivery, sent []byte, enrollment bool, generation *uint64, expiresAt int64, warning string) {
	if len(deliveries) == 0 {
		http.Error(w, "no native deliveries supplied", http.StatusBadRequest)
		return
	}
	for i := range deliveries {
		raw, err := mailmsg.NormalizeSMTPMessage(deliveries[i].Raw)
		if err != nil {
			http.Error(w, "native message cannot be submitted; correct the MIME size or line formatting", http.StatusBadRequest)
			return
		}
		deliveries[i].Raw = raw
	}
	job := mailbox.OutboundJob{From: from, Deliveries: deliveries, Sent: sent, DeviceID: ac.DeviceID, DeviceWitness: ac.DeviceWitness, NativeSendEpoch: ac.NativeSendEpoch, PGPRevision: u.PGPRevision, RequiresEnrollment: enrollment, ExpiresAt: expiresAt}
	if generation != nil {
		job.MaterialGeneration = *generation
	} else if u.PGPKeyring != nil && !enrollment {
		job.MaterialGeneration = u.PGPKeyring.MaterialGeneration
	}
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		http.Error(w, "cannot create native delivery intent; retry later", http.StatusServiceUnavailable)
		return
	}
	sender := s.nativeOutbound()
	first, sendErr := sender.Send(r.Context(), ac.UserID, id, job)
	if !first.Accepted {
		message := "native submission was not confirmed; inspect this outbox job before resubmitting to avoid duplicates"
		if errors.Is(sendErr, sso.ErrNativeOutboundStale) || errors.Is(sendErr, sso.ErrNativeProvisioning) {
			message = "native sender authority changed; reload account and domain setup before sending"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": message, "outboxId": id, "ok": false})
		return
	}
	if sendErr != nil {
		warning = joinWarnings(warning, "relay accepted the primary message but completion failed; do not resubmit it")
	}
	unconfirmed := 0
	for sequence := 1; sequence < len(deliveries); sequence++ {
		result, err := sender.Submit(r.Context(), ac.UserID, id, sequence)
		if !result.Accepted || err != nil {
			unconfirmed++
		}
	}
	if unconfirmed > 0 {
		warning = joinWarnings(warning, "some follow-on deliveries are pending or unconfirmed; inspect the outbox before resubmitting")
	}
	if !first.SentSaved && len(sent) > 0 {
		finish, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		first.SentSaved = sender.FileSent(finish, ac.UserID, id) == nil
		cancel()
	}
	if !first.SentSaved && len(sent) > 0 {
		warning = joinWarnings(warning, "email accepted but Sent filing remains pending; sending it again will not repair Sent")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentSaved": first.SentSaved, "warning": warning, "outboxId": id})
}

func nativeFromAllowed(primary, requested string) bool {
	return strings.TrimSpace(requested) == "" || strings.EqualFold(strings.TrimSpace(requested), primary)
}

// The diagnostics response contains state only, never decrypted intent or device credentials.
func (s *Server) handleNativeOutboxStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if !s.nativeMail {
		http.Error(w, "native outbox requires native mail mode", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if len(id) != 36 || !fsutil.SafePathComponent(id) {
		http.Error(w, "invalid outbox id", http.StatusBadRequest)
		return
	}
	statuses, sentSaved, err := s.nativeOutbound().Status(r.Context(), ac.UserID, id)
	if err != nil {
		http.Error(w, "native outbox unavailable; retain mail and repair storage before resubmitting", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outboxId": id, "deliveries": statuses, "sentSaved": sentSaved})
}
