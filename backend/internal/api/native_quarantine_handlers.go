package api

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// openHolding opens the receiving spool; nil without error when receiving was
// never initialized.
func (s *Server) openHolding() (*ingress.Store, error) {
	dir := filepath.Join(s.stateDir, "receiving")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return ingress.OpenExisting(dir, ingress.ReceivingLimits)
}

// handleCloudflareReceivingStatus reports the continuous Cloudflare profile:
// state, revisions, times and counts, never addresses or envelopes.
func (s *Server) handleCloudflareReceivingStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	status, err := cfreceiving.CurrentStatus(r.Context(), cfreceiving.Keys{Dir: config.SecretDir()}, filepath.Join(s.stateDir, "receiving"))
	if err != nil {
		http.Error(w, "cloudflare receiving state unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleQuarantineList pages quarantined envelopes (?after=<sequence>), never
// bodies, subjects or headers.
func (s *Server) handleQuarantineList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	after, err := int64(0), error(nil)
	if v := r.URL.Query().Get("after"); v != "" {
		after, err = strconv.ParseInt(v, 10, 64)
	}
	if err != nil || after < 0 {
		http.Error(w, "invalid after", http.StatusBadRequest)
		return
	}
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	out := []sso.QuarantinedDelivery{}
	holding, err := s.openHolding()
	if err == nil && holding != nil {
		defer holding.Close()
		out, err = s.ssoLifecycle.QuarantinedDeliveries(r.Context(), holding, after)
	}
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil {
		http.Error(w, "receiving store unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (s *Server) handleQuarantineRelease(w http.ResponseWriter, r *http.Request) {
	s.changeQuarantine(w, r, "release_quarantine", "released")
}

func (s *Server) handleQuarantineDiscard(w http.ResponseWriter, r *http.Request) {
	s.changeQuarantine(w, r, "discard_quarantine", "discarded")
}

// changeQuarantine is step-up confirmed and audited with the delivery ID only.
// {"toCurrentOwner": true} releases an unresolved delivery (original owner
// unknown) to its address's current owner; the step-up binds the flag, and the
// audit names that action.
func (s *Server) changeQuarantine(w http.ResponseWriter, r *http.Request, action, done string) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		ToCurrentOwner bool `json:"toCurrentOwner"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	if body.ToCurrentOwner && done != "released" {
		http.Error(w, "invalid mail administration request", http.StatusBadRequest)
		return
	}
	if body.ToCurrentOwner {
		action = "release_quarantine_to_current_owner"
	}
	ac, _ := authFromContext(r)
	gateway, id := r.PathValue("gateway"), r.PathValue("id")
	result := "refused"
	// Malformed path values are neither logged nor looked up.
	valid := ingress.ValidIdentifier(gateway) && ingress.ValidIdentifier(id)
	if !valid {
		gateway, id = "", ""
	}
	defer func() {
		s.logger.Info("receiving quarantine change", "actor", ac.UserID, "task_id", "native-receiving", "action", action, "target", gateway+"/"+id, "result", result, "correlation_id", id)
	}()
	if !s.nativeMail || !valid {
		http.Error(w, "quarantined delivery not found", http.StatusNotFound)
		return
	}
	holding, err := s.openHolding()
	if err == nil && holding == nil {
		err = sql.ErrNoRows
	}
	if err == nil {
		defer holding.Close()
		if done == "released" {
			// A disabled SSO configuration has no issuer, so admission refuses.
			issuer := ""
			if settings := s.ssoStore.Load(); settings.Enabled {
				issuer = settings.IssuerURL
			}
			err = s.ssoLifecycle.ReleaseQuarantined(r.Context(), s.stateDir, issuer, s.users, holding, gateway, id, body.ToCurrentOwner)
		} else if err = sso.RequireNativeRestoreReleased(s.stateDir); err == nil {
			// partially_released: an interrupted release may have reached
			// some frozen mailboxes before this discard.
			done, err = holding.Discard(r.Context(), gateway, id)
		}
	}
	switch {
	case err == nil:
		result = done
		writeJSON(w, http.StatusOK, map[string]any{"gateway": gateway, "id": id, "result": done})
	case nativeMigrationRefused(w, err):
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "quarantined delivery not found", http.StatusNotFound)
	case errors.Is(err, ingress.ErrNotQuarantined), errors.Is(err, ingress.ErrLease), errors.Is(err, sso.ErrQuarantineRelease), errors.Is(err, sso.ErrNativeRestoreHold):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "quarantine change refused; the holding copy is retained. Check mailbox capacity and storage, then retry", http.StatusServiceUnavailable)
	}
}
