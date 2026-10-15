package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const reasonQuota = "your sending quota was reached"

var errSendQuotaUnavailable = errors.New("sending quota unavailable")

// sendQuotaFor is the account's override, else the install-wide quota. A
// hand-edited zero falls back to the default rather than to no limit.
func (s *Server) sendQuotaFor(userID string) (config.SendQuota, error) {
	u, err := s.users.Get(userID)
	if err != nil {
		return config.SendQuota{}, err
	}
	if u.SendQuota != nil && u.SendQuota.Valid() {
		return *u.SendQuota, nil
	}
	s.cfgMu.RLock()
	q := s.cfg.SendQuota
	s.cfgMu.RUnlock()
	if !q.Valid() {
		q = config.Default().SendQuota
	}
	return q, nil
}

// chargeSend counts one message to recipients against the user's quota. id
// keeps a retried send from being charged twice. It fails closed: without the
// ledger nothing is sent.
func (s *Server) chargeSend(ctx context.Context, id, userID string, recipients int, now time.Time) (time.Duration, error) {
	if s.globalStore == nil {
		return 0, errSendQuotaUnavailable
	}
	q, err := s.sendQuotaFor(userID)
	if err != nil {
		return 0, err
	}
	return s.globalStore.AdmitSend(ctx, id, userID, recipients, q.MessagesPerHour, q.RecipientsPerDay, now.Unix())
}

// admitSend charges an immediate send, answering the client itself when the
// send may not go out. A charged send that then fails still counts, so a
// failing relay cannot be hammered past the quota.
func (s *Server) admitSend(w http.ResponseWriter, ctx context.Context, userID string, recipients int) bool {
	id, err := fsutil.NewUUIDv4()
	if err != nil {
		http.Error(w, "sending unavailable; retry later", http.StatusServiceUnavailable)
		return false
	}
	wait, err := s.chargeSend(ctx, id, userID, recipients, time.Now())
	switch {
	case err == nil:
		return true
	case errors.Is(err, state.ErrSendQuota):
		s.logger.Warn("sending quota reached", "user_id", userID)
		seconds := int(wait.Seconds())
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":             fmt.Sprintf("sending quota reached; try again in about %d minutes", (seconds+59)/60),
			"retryAfterSeconds": seconds,
		})
	case errors.Is(err, state.ErrSendTooLarge):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
	case errors.Is(err, users.ErrNotFound):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	default:
		s.logger.Error("sending quota check failed", "error", err.Error())
		http.Error(w, "sending unavailable; retry later", http.StatusServiceUnavailable)
	}
	return false
}

// handleUserSendQuota sets or clears (null) one account's quota override.
func (s *Server) handleUserSendQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SendQuota *config.SendQuota `json:"sendQuota"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.SendQuota != nil && !req.SendQuota.Valid() {
		http.Error(w, "sending quota limits must be greater than zero", http.StatusBadRequest)
		return
	}
	u, err := s.users.SetSendQuota(r.PathValue("id"), req.SendQuota)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	s.logger.Info("user send quota updated", "user_id", u.ID)
	writeJSON(w, http.StatusOK, u.Public())
}
