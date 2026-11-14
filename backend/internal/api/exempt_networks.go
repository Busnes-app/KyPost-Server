package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Busnes-app/ky-primitives/netallow"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// exemptNetworksSetting is the admin's half of the office-network allowlist,
// in root state.db's meta table. RATE_LIMIT_EXEMPT_CIDRS is the other half.
const exemptNetworksSetting = "rate_limit_exempt_networks"

// perIPExempt reports whether r comes from an allowlisted office network and so
// skips the per-IP sign-in limits: the PoW challenge budget, login-params, the
// SSO start/callback meter and loginIPLockout. Only those. Per-account
// lockouts, the instance-wide login budget, the CAPTCHA, PoW difficulty, MFA
// and SSO state checks still run. It matches clientIP, so a forwarding header
// counts only from a trusted proxy, and only its right-most hop.
func (s *Server) perIPExempt(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	return err == nil && s.exemptNetworks.Load().Exempt(addr)
}

// storedExemptNetworks reads the admin's list; unset reads as empty.
func (s *Server) storedExemptNetworks() (string, error) {
	if s.globalStore == nil {
		return "", errors.New("the server state database did not open")
	}
	v, err := s.globalStore.Setting(exemptNetworksSetting)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// loadExemptNetworks builds the live list at construction. A stored list that
// cannot be read or no longer parses is dropped for the environment's list:
// fewer exemptions, never more.
func (s *Server) loadExemptNetworks() {
	env, err := config.LoadRateLimitExemptCIDRs()
	if err != nil {
		// app.Run refuses this at startup; only a caller that skipped it lands here.
		s.logger.Error("rate-limit exempt networks ignored", "error", err.Error())
	}
	s.exemptNetworksEnv = env
	var list *netallow.List
	stored, err := s.storedExemptNetworks()
	if err == nil {
		list, err = netallow.Parse(env + "," + stored)
	}
	if err != nil {
		if s.globalStore != nil {
			s.logger.Error("stored rate-limit exempt networks ignored; using the environment's only", "error", err.Error())
		}
		list, _ = netallow.Parse(env)
	}
	s.exemptNetworks.Store(list)
}

func (s *Server) exemptNetworksStatus(stored string) map[string]any {
	return map[string]any{
		"networks":    stored,
		"environment": s.exemptNetworksEnv,
		"exempted":    s.exemptNetworks.Load().Exempted(),
	}
}

// handleExemptNetworksGet is GET /api/admin/rate-limit-networks.
func (s *Server) handleExemptNetworksGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	stored, err := s.storedExemptNetworks()
	if err != nil && s.globalStore != nil {
		http.Error(w, "could not read the office networks", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.exemptNetworksStatus(stored))
}

// handleExemptNetworksPut is PUT /api/admin/rate-limit-networks: validate,
// store the canonical form, swap the live list, audit. The environment's half
// is not editable here. The exempted count restarts with the new list.
func (s *Server) handleExemptNetworksPut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Networks string `json:"networks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	ac, _ := authFromContext(r)
	digest, _ := r.Context().Value(actionDigestKey{}).(string)
	audit := func(result, detail string) {
		s.logger.Info("rate-limit exempt networks change", "actor", ac.UserID, "task_id", "rate-limit-exempt", "action", "set_exempt_networks", "target", exemptNetworksSetting, "result", result, "correlation_id", digest, "detail", detail)
	}
	own, err := netallow.Parse(req.Networks)
	var combined *netallow.List
	if err == nil {
		combined, err = netallow.Parse(s.exemptNetworksEnv + "," + own.String())
	}
	if err != nil {
		audit("refused", "")
		http.Error(w, strings.TrimPrefix(err.Error(), "netallow: "), http.StatusBadRequest)
		return
	}
	if s.globalStore == nil {
		http.Error(w, "office networks are unavailable: the server state database did not open", http.StatusServiceUnavailable)
		return
	}
	previous, _ := s.storedExemptNetworks()
	stored := own.String()
	if err := s.globalStore.SetSetting(exemptNetworksSetting, stored); err != nil {
		s.logger.Error("saving rate-limit exempt networks failed", "error", err.Error())
		http.Error(w, "could not save the office networks", http.StatusInternalServerError)
		return
	}
	s.exemptNetworks.Store(combined)
	audit("saved", fmt.Sprintf("from=%q to=%q", previous, stored))
	writeJSON(w, http.StatusOK, s.exemptNetworksStatus(stored))
}
