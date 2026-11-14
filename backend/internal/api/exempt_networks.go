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

// Office budgets. A request from an allowlisted office network skips a per-IP
// limit only by drawing one request from that limit's shared office budget:
// one bucket per limiter, shared by every listed network. Once it is spent the
// ordinary per-IP limit applies to office requests again, so one host inside
// an office cannot make unbounded attempts.
const (
	// officeBudgetKey is the single key of each office budget.
	officeBudgetKey = "office"

	// officeRequestsPerMinute is the office budget for the PoW challenge
	// budget and for login-params and the SSO meter, which share one bucket:
	// ten times the 30-a-minute address burst, enough for 3,000 staff
	// arriving over ten minutes. Neither route has an instance-wide budget,
	// and neither writes per-account state, so nothing caps this from above.
	officeRequestsPerMinute = 300

	// officeLoginBurst/officeLoginRefillPerSec are the office budget standing
	// in for loginIPLockout, in failures: the instance login budget's own
	// burst and its steady-state rate at the reference derivation cost
	// (loginKDFDutyCycle / loginKDFReserveSeconds, one a second), so the
	// office can never be allowed more than the instance budget admits.
	// Sustained for loginLockoutFor it writes at most 915 per-account
	// lockout keys, plus the 50 the address lock allows after it: under a
	// tenth of loginLockoutSweepThreshold and far below loginLockoutHardCap,
	// so office churn alone cannot evict the live strikes of other accounts.
	// Pinned by TestOfficeBudgetsFitTheLimiters.
	officeLoginBurst        = loginRateBurst
	officeLoginRefillPerSec = loginKDFDutyCycle / loginKDFReserveSeconds
)

func newOfficeRequestBudget() *keyedLimiter {
	return newKeyedLimiter(officeRequestsPerMinute, officeRequestsPerMinute/60.0)
}

func newOfficeLoginBudget() *keyedLimiter {
	return newKeyedLimiter(officeLoginBurst, officeLoginRefillPerSec)
}

// officeDraw reports whether r comes from an allowlisted office network AND
// took one request from budget, the office budget of one per-IP limit; only
// then does the caller skip that per-IP limit. Only per-IP limits ask:
// per-account lockouts, the instance login budget, the CAPTCHA, PoW
// difficulty, MFA and SSO state checks run regardless. It matches clientIP, so
// a forwarding header counts only from a trusted proxy, and only its right-most
// hop. A request counts as exempted only when it skipped the per-IP limit.
func (s *Server) officeDraw(r *http.Request, budget *keyedLimiter) bool {
	list := s.exemptNetworks.Load()
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil || budget == nil || !list.Contains(addr) {
		return false
	}
	if ok, _ := budget.allow(officeBudgetKey); !ok {
		return false
	}
	list.Exempt(addr)
	return true
}

// officeRefund hands back one request officeDraw took from budget, for an
// attempt that turned out not to be a failure.
func officeRefund(budget *keyedLimiter) {
	budget.settleCost(officeBudgetKey, -1)
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
