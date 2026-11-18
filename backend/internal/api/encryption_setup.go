package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// First sign-in encryption setup and the admin policy that enforces it
// (docs/E2E_PGP.md, "First sign-in setup"). The server only decides who must
// finish setup and refuses mail until they have; the key itself is still
// generated, wrapped and recovered in the browser, as before.

const (
	policyRequired = "required"
	policyOptional = "optional"
	policyOn       = "on"
	policyOff      = "off"

	encryptionPolicySetting = "encryption_policy"
)

// EncryptionPolicy is Admin → Server → Security. Key is required|optional;
// Incoming is required|on|off, where on/off only preselect the setup choice.
type EncryptionPolicy struct {
	Key      string `json:"key"`
	Incoming string `json:"incoming"`
}

// shippedEncryptionPolicy is the default an install without a saved policy
// runs. defaultEncryptionPolicy is what the code reads; only TestMain lowers it,
// so the package's unrelated tests are not all sent through setup.
var (
	shippedEncryptionPolicy = EncryptionPolicy{Key: policyRequired, Incoming: policyOn}
	defaultEncryptionPolicy = shippedEncryptionPolicy
)

func (p EncryptionPolicy) valid() bool {
	return (p.Key == policyRequired || p.Key == policyOptional) &&
		(p.Incoming == policyRequired || p.Incoming == policyOn || p.Incoming == policyOff) &&
		(p.Incoming != policyRequired || p.Key == policyRequired)
}

func (p EncryptionPolicy) anyRequired() bool {
	return p.Key == policyRequired || p.Incoming == policyRequired
}

// requiredNames names the settings an administrator must lower first.
func (p EncryptionPolicy) requiredNames() string {
	var names []string
	if p.Key == policyRequired {
		names = append(names, "Encryption key")
	}
	if p.Incoming == policyRequired {
		names = append(names, "Encrypt incoming mail")
	}
	return strings.Join(names, " and ")
}

// encryptionPolicy is the saved policy, or the default when none is saved or
// it cannot be read: an unreadable policy never relaxes the requirement.
func (s *Server) encryptionPolicy() EncryptionPolicy {
	if s.globalStore == nil {
		return defaultEncryptionPolicy
	}
	raw, err := s.globalStore.Setting(encryptionPolicySetting)
	var p EncryptionPolicy
	if err == nil && json.Unmarshal([]byte(raw), &p) == nil && p.valid() {
		return p
	}
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		s.logger.Error("encryption policy unreadable; using the default", "error", err.Error())
	}
	return defaultEncryptionPolicy
}

// encryptionSetup is one account's setup state, reported by /api/auth/me.
type encryptionSetup struct {
	// Required: mail is refused until setup is complete.
	Required bool `json:"required"`
	// Offered: the step is offered once, with "Not now".
	Offered         bool   `json:"offered"`
	HasKey          bool   `json:"hasKey"`
	HasRecovery     bool   `json:"hasRecovery"`
	IncomingEnabled bool   `json:"incomingEnabled"`
	KeyPolicy       string `json:"keyPolicy"`
	IncomingPolicy  string `json:"incomingPolicy"`
}

// canSetUpKey reports whether u can create a key here: with its own password,
// or as an SSO-only account through KySignOn, the only provider that can
// confirm a key write for it (requirePGPStepUp). Anyone else is never sent
// through setup, because they could not finish it.
func canSetUpKey(u users.User, kySignOn bool) bool {
	return u.HasLocalCredential() || kySignOn
}

// encryptionSetupFor computes u's setup. Complete means a client-protected key
// with a server recovery copy (which the browser stores only after the user
// confirms saving its secret) and, under a Required incoming policy, incoming
// encryption turned on.
func (s *Server) encryptionSetupFor(u users.User, canSetUp bool) (encryptionSetup, error) {
	p := s.encryptionPolicy()
	st := encryptionSetup{
		KeyPolicy:      p.Key,
		IncomingPolicy: p.Incoming,
		HasKey:         u.PGPProtection() == users.PGPProtectionClient && u.PGPPrivateKeyWrapped != "",
		HasRecovery: slices.ContainsFunc(u.PGPWrappedEnvelopes, func(e users.WrappedEnvelope) bool {
			return e.Slot == users.EnvelopeSlotRecovery
		}),
	}
	settings, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
	if err != nil {
		return st, err
	}
	st.IncomingEnabled = settings.EncryptIncoming
	if !canSetUp {
		return st, nil
	}
	must := p.anyRequired() || u.KeySetupRequired
	done := st.HasKey && st.HasRecovery
	if done && p.Incoming == policyRequired && !st.IncomingEnabled {
		// Incoming encryption covers only the primary mailbox and is refused
		// beside an active extra one, so its owner is not held to it.
		if done, err = s.ownsActiveExtraMailbox(u.ID); err != nil {
			return st, err
		}
	}
	st.Required = must && !done
	st.Offered = !must && !st.HasKey
	return st, nil
}

// refuseUntilEncryptionSetup answers 403 encryption_setup_required while the
// caller's account owes setup. A browser session is held only when it was
// signed in while setup was owed (Session.KeySetupGate), so a policy change
// never cuts off a session mid-work; any other caller (paired device, agent
// token) is checked on every request and cannot read mail around it.
func (s *Server) refuseUntilEncryptionSetup(w http.ResponseWriter, ac AuthContext) bool {
	browser := ac.SessionCSRFToken != ""
	if browser && !ac.KeySetupGate {
		return false
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "encryption setup status is unavailable"})
		return true
	}
	kySignOn := ac.SSOSession
	if !browser && !u.HasLocalCredential() {
		cfg := s.ssoStore.Load()
		kySignOn = cfg.Enabled && cfg.KySignOn
	}
	st, err := s.encryptionSetupFor(u, canSetUpKey(u, kySignOn))
	if err != nil {
		s.logger.Error("encryption setup status unavailable", "user_id", u.ID, "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "encryption setup status is unavailable"})
		return true
	}
	if !st.Required {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   "encryption_setup_required",
		"message": "Finish email encryption setup in KyPost on the web before using mail here.",
	})
	return true
}

// detectKySignOn asks the issuer's discovery document (cached by
// sso.NewProvider) whether cfg's provider is KySignOn.
func (s *Server) detectKySignOn(ctx context.Context, cfg sso.SSOSettings) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := sso.NewProvider(ctx, cfg, s.ssoRedirectURI())
	if err != nil {
		return false, err
	}
	return p.KySignOn(), nil
}

// refreshSSOKySignOn confirms by discovery whether an enabled provider not yet
// marked KySignOn is one, and records a positive answer. Settings written
// before detection existed carry no mark; a failed check changes nothing.
func (s *Server) refreshSSOKySignOn(ctx context.Context) sso.SSOSettings {
	s.encPolicyMu.Lock()
	defer s.encPolicyMu.Unlock()
	cfg := s.ssoStore.Load()
	if !cfg.Enabled || cfg.KySignOn {
		return cfg
	}
	if ok, _ := s.detectKySignOn(ctx, cfg); ok {
		cfg.KySignOn = true
		if err := s.ssoStore.Save(cfg); err != nil {
			s.logger.Error("recording the KySignOn provider failed", "error", err.Error())
		}
	}
	return cfg
}

// needsKySignOn is the refusal for Required beside a generic provider.
func needsKySignOn(cfg sso.SSOSettings, detectErr error) string {
	provider := cfg.IssuerURL
	if u, err := url.Parse(cfg.IssuerURL); err == nil && u.Host != "" {
		provider = u.Host
	}
	msg := "Needs KySignOn: users signing in through " + provider + " couldn't create a key and would be locked out."
	if detectErr != nil {
		msg += " KyPost could not confirm the provider is KySignOn: " + detectErr.Error()
	}
	return msg
}

func encryptionPolicyView(p EncryptionPolicy, cfg sso.SSOSettings) map[string]any {
	view := map[string]any{"key": p.Key, "incoming": p.Incoming, "genericProvider": cfg.Enabled && !cfg.KySignOn}
	if cfg.Enabled {
		view["issuer"] = cfg.IssuerURL
	}
	return view
}

func (s *Server) handleEncryptionPolicyGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.refreshSSOKySignOn(r.Context())
	writeJSON(w, http.StatusOK, encryptionPolicyView(s.encryptionPolicy(), cfg))
}

// handleEncryptionPolicyPut saves the policy. Required is refused while a
// provider other than KySignOn is configured; Required incoming encryption
// sets the key requirement too, since mail cannot be encrypted to no key.
func (s *Server) handleEncryptionPolicyPut(w http.ResponseWriter, r *http.Request) {
	var req EncryptionPolicy
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Incoming == policyRequired {
		req.Key = policyRequired
	}
	if !req.valid() {
		http.Error(w, "key must be required or optional; incoming must be required, on or off", http.StatusBadRequest)
		return
	}
	ac, _ := authFromContext(r)
	s.encPolicyMu.Lock()
	defer s.encPolicyMu.Unlock()
	cfg := s.ssoStore.Load()
	if req.anyRequired() && cfg.Enabled && !cfg.KySignOn {
		ok, err := s.detectKySignOn(r.Context(), cfg)
		if !ok {
			writeJSON(w, http.StatusConflict, map[string]any{"error": needsKySignOn(cfg, err), "code": "kysignon_required"})
			return
		}
		cfg.KySignOn = true
		if err := s.ssoStore.Save(cfg); err != nil {
			http.Error(w, "could not record the KySignOn provider; the policy was not changed", http.StatusInternalServerError)
			return
		}
	}
	if s.backup == nil || s.globalStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the audit log is unavailable; the policy was not changed"})
		return
	}
	prev := s.encryptionPolicy()
	details := map[string]any{"key_from": prev.Key, "key_to": req.Key, "incoming_from": prev.Incoming, "incoming_to": req.Incoming}
	if !s.auditOrRefuse(w, "admin.encryption_policy", ac.UserID, "server", details) {
		return
	}
	raw, _ := json.Marshal(req)
	if err := s.globalStore.SetSetting(encryptionPolicySetting, string(raw)); err != nil {
		_ = s.backup.Audit("admin.encryption_policy", ac.UserID, "server", "failed", details)
		http.Error(w, "could not save the encryption policy", http.StatusInternalServerError)
		return
	}
	if err := s.backup.Audit("admin.encryption_policy", ac.UserID, "server", "success", details); err != nil {
		s.logger.Error("encryption policy audit outcome not recorded", "error", err.Error())
	}
	s.logger.Info("encryption policy changed", "actor", ac.UserID, "action", "set_encryption_policy", "result", "saved", "detail", "key="+req.Key+" incoming="+req.Incoming)
	writeJSON(w, http.StatusOK, encryptionPolicyView(req, cfg))
}

// auditOrRefuse records an admin change's intent before it is made, and
// refuses the change when the audit log cannot take it.
func (s *Server) auditOrRefuse(w http.ResponseWriter, action, actor, target string, details map[string]any) bool {
	if err := s.backup.Audit(action, actor, target, "started", details); err != nil {
		s.logger.Error("audit write failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the audit log is unavailable; nothing was changed"})
		return false
	}
	return true
}

// handleUserKeySetup is "Require key setup": it sets or clears a flag and
// nothing else. The user still creates or imports the key on their own device;
// no administrator route reads, writes or returns key material.
func (s *Server) handleUserKeySetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Required *bool `json:"required"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || req.Required == nil {
		http.Error(w, "invalid request: required must be true or false", http.StatusBadRequest)
		return
	}
	ac, _ := authFromContext(r)
	id := r.PathValue("id")
	if _, err := s.users.Get(id); err != nil {
		writeUserStoreError(w, err)
		return
	}
	if s.backup == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the audit log is unavailable; nothing was changed"})
		return
	}
	details := map[string]any{"required": *req.Required}
	if !s.auditOrRefuse(w, "admin.require_key_setup", ac.UserID, "user:"+id, details) {
		return
	}
	u, err := s.users.SetKeySetupRequired(id, *req.Required)
	if err != nil {
		_ = s.backup.Audit("admin.require_key_setup", ac.UserID, "user:"+id, "failed", details)
		writeUserStoreError(w, err)
		return
	}
	if err := s.backup.Audit("admin.require_key_setup", ac.UserID, "user:"+id, "success", details); err != nil {
		s.logger.Error("key setup audit outcome not recorded", "error", err.Error())
	}
	s.logger.Info("user key setup requirement updated", "user_id", u.ID, "detail", "required="+strconv.FormatBool(*req.Required))
	writeJSON(w, http.StatusOK, u.Public())
}
