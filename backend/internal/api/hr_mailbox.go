package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func (s *Server) handleHRMailbox(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Off in multi mode (tenancy K6): its body names a subject with no org_id.
	if s.multiTenant() {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 4096 {
		http.Error(w, "invalid reservation", http.StatusBadRequest)
		return
	}
	event, settings, ok := s.verifySyncEvent(r, body)
	if !ok || event.Type != "hr.mailbox.reserve" || !settings.Enabled || settings.IssuerURL == "" {
		http.Error(w, "unauthorized reservation", http.StatusUnauthorized)
		return
	}
	if !s.nativeMail {
		http.Error(w, "native mail is required", http.StatusConflict)
		return
	}
	var req struct {
		UserID    string `json:"userId"`
		Preferred string `json:"preferredName"`
		Domain    string `json:"domain"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid reservation", http.StatusBadRequest)
		return
	}
	// An HR request must never attach a mailbox to an existing local or
	// administrator account. Repeated reservations for native owners are safe.
	u, err := s.users.GetBySSOSubIssuer(settings.IssuerURL, req.UserID)
	if err != nil && !errors.Is(err, users.ErrNotFound) || err == nil && (u.Role == users.RoleAdmin || u.NativeMailboxSource == "") {
		http.Error(w, "identity cannot reserve a mailbox", http.StatusConflict)
		return
	}
	address, err := s.ssoLifecycle.ReserveHRAddress(r.Context(), s.stateDir, settings.IssuerURL, req.UserID, req.Preferred, req.Domain, s.nativeDomains, nativeMailboxLimits)
	if err != nil {
		s.logger.Info("HR address reservation refused", "event_id", event.ID)
		http.Error(w, "address reservation unavailable; check native domain, ownership and restore status", http.StatusConflict)
		return
	}
	s.logger.Info("HR address reserved", "event_id", event.ID, "target", req.UserID)
	writeJSON(w, http.StatusOK, map[string]string{"userId": req.UserID, "email": address})
}
