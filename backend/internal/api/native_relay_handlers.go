package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func (s *Server) handleNativeMailRelay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := filepath.Join(s.configDir, "native-relay.json")
	keyPath := filepath.Join(config.SecretDir(), "native-relay.key")
	respond := func(c mailmsg.DomainRelay, exists bool) {
		// "domain" is the founding domain the single-domain UI pairs with its claim.
		domain := ""
		if exists {
			domain = c.Domains[0]
			if founding, err := s.nativeDomains.Read(); err == nil && c.Sends(founding.Domain) {
				domain = founding.Domain
			}
		}
		// Neither relay authentication username nor password is a read API field.
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": exists, "domain": domain, "domains": append([]string{}, c.Domains...),
			"retiredDomains": append([]string{}, c.RetiredDomains...), "issuer": c.Issuer,
			"host": c.Host, "port": c.Port, "generation": c.Generation,
			"transport": "implicit-tls", "authRequired": true, "sendingEnabled": s.nativeMail && exists,
		})
	}
	if r.Method == http.MethodGet {
		c, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
		if err != nil {
			http.Error(w, mailmsg.ErrDomainRelay.Error(), http.StatusServiceUnavailable)
			return
		}
		respond(c, exists)
		return
	}
	var body struct {
		Host         string   `json:"host"`
		Port         int      `json:"port"`
		SMTPUsername string   `json:"smtpUsername"`
		SMTPPassword string   `json:"smtpPassword"`
		Domains      []string `json:"domains"`
		Password     string   `json:"password"`
		AuthSecret   string   `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	if err := sso.RequireNativeRestoreReleased(s.stateDir); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	settings := s.ssoStore.Load()
	if !settings.Enabled || settings.IssuerURL == "" {
		http.Error(w, "enable KyIdentity and prove the mail domain before configuring a relay", http.StatusConflict)
		return
	}
	// Domains alone change only the set and keep the relay generation.
	domainsOnly := body.Domains != nil && body.Host == "" && body.Port == 0 && body.SMTPUsername == "" && body.SMTPPassword == ""
	prior, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
	if err != nil {
		http.Error(w, mailmsg.ErrDomainRelay.Error(), http.StatusConflict)
		return
	}
	domains := body.Domains
	if domains == nil && exists {
		domains = prior.Domains
	} else if domains == nil {
		founding, err := s.nativeDomains.Read()
		if nativeMigrationRefused(w, err) {
			return
		}
		domains = []string{founding.Domain}
	}
	domains = slices.Compact(slices.Sorted(slices.Values(domains)))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	proofs := make([]sso.NativeDomain, 0, len(domains))
	for _, domain := range domains {
		proof, err := s.nativeDomains.VerifyDomain(ctx, domain) // DNS precedes all disk fences.
		if nativeMigrationRefused(w, err) {
			return
		}
		if err != nil || proof.Issuer != settings.IssuerURL {
			http.Error(w, "relay setup requires fresh issuer-bound proof of every relay domain; verify the current TXT records", http.StatusConflict)
			return
		}
		proofs = append(proofs, proof)
	}
	if len(proofs) == 0 {
		http.Error(w, "select at least one verified relay domain", http.StatusBadRequest)
		return
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, sso.NativeDomainsFile))
	if err != nil {
		http.Error(w, "relay setup busy or cancelled; retry without deleting configuration", http.StatusServiceUnavailable)
		return
	}
	defer release()
	current, err := s.nativeDomains.ReadSet()
	currentSettings := s.ssoStore.Load()
	again, againExists, againErr := mailmsg.ReadDomainRelay(path, keyPath)
	changed := err != nil || againErr != nil || againExists != exists || !again.Equal(prior) || !currentSettings.Enabled || currentSettings.IssuerURL != settings.IssuerURL
	for _, proof := range proofs {
		changed = changed || !current.CurrentProof(proof)
	}
	if changed {
		http.Error(w, "mail-domain authority or relay changed during relay setup; reverify and retry", http.StatusConflict)
		return
	}
	if slices.ContainsFunc(prior.Domains, func(d string) bool { return !slices.Contains(domains, d) }) {
		queued, err := sso.NativeQueuedFromDomains(ctx, s.configDir, s.stateDir, keyPath)
		if err != nil {
			http.Error(w, "outbox unavailable; preserve queued mail and retry removing the relay domain", http.StatusServiceUnavailable)
			return
		}
		if slices.ContainsFunc(prior.Domains, func(d string) bool { return queued[d] && !slices.Contains(domains, d) }) {
			http.Error(w, "a relay domain still has queued or retryable outbox jobs; let them finish before removing it", http.StatusConflict)
			return
		}
	}
	if err := sso.RequireNativeRestoreReleased(s.stateDir); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	var c mailmsg.DomainRelay
	if domainsOnly {
		c, err = mailmsg.SetDomainRelayDomains(ctx, path, keyPath, domains)
	} else {
		if body.Port == 0 {
			body.Port = 465
		}
		c, err = mailmsg.SaveDomainRelay(ctx, path, keyPath, mailmsg.DomainRelay{
			Domains: domains, Issuer: settings.IssuerURL, Host: body.Host, Port: body.Port, Username: body.SMTPUsername, Password: body.SMTPPassword,
		})
	}
	if err != nil {
		http.Error(w, mailmsg.ErrDomainRelay.Error(), http.StatusConflict)
		return
	}
	respond(c, true)
}

// A check uses only the saved profile and never creates mail or outbox state.
func (s *Server) handleNativeMailRelayTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		ExpectedGeneration string `json:"expectedGeneration"`
		Password           string `json:"password"`
		AuthSecret         string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	if len(body.ExpectedGeneration) != 36 || !fsutil.SafePathComponent(body.ExpectedGeneration) {
		http.Error(w, "select a saved relay generation before checking it", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	profile, proofs, err := s.nativeRelayCheckProfile(ctx, body.ExpectedGeneration, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// ponytail: one profile per instance, so one fixed cooldown key bounds probes.
	if ok, _ := s.nativeRelayCheckCooldown.tryConsume("native-relay"); !ok {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "relay check cooling down; wait 30 seconds before retrying", http.StatusTooManyRequests)
		return
	}
	// All authority fences have been released before contacting the provider.
	if err := profile.Check(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if _, _, err := s.nativeRelayCheckProfile(ctx, body.ExpectedGeneration, proofs); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generation": profile.Generation, "host": profile.Host, "port": profile.Port,
		"tls": true, "authenticated": true, "deliveryTested": false,
	})
}

// DNS runs before domain -> settings -> relay disk fences. After network I/O,
// stable challenge identity of every relay domain must still match even though
// Verify renews its cache.
func (s *Server) nativeRelayCheckProfile(ctx context.Context, generation string, prior []sso.NativeDomain) (mailmsg.DomainRelay, []sso.NativeDomain, error) {
	refused := errors.New("relay check authority or saved profile changed; verify the current TXT record, issuer and restore status, then reload the saved relay")
	path, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	profile, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
	if err != nil {
		return mailmsg.DomainRelay{}, nil, mailmsg.ErrDomainRelay
	}
	if !exists || profile.Generation != generation || sso.RequireNativeRestoreReleased(s.stateDir) != nil || prior != nil && len(prior) != len(profile.Domains) {
		return mailmsg.DomainRelay{}, nil, refused
	}
	proofs := make([]sso.NativeDomain, 0, len(profile.Domains))
	for i, domain := range profile.Domains {
		proof, err := s.nativeDomains.VerifyDomain(ctx, domain)
		if err != nil || !proof.Established || proof.Issuer != profile.Issuer ||
			prior != nil && (proof.Domain != prior[i].Domain || proof.Issuer != prior[i].Issuer || proof.Token != prior[i].Token || proof.ExpiresAt != prior[i].ExpiresAt) {
			return mailmsg.DomainRelay{}, nil, refused
		}
		proofs = append(proofs, proof)
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, sso.NativeDomainsFile))
	if err != nil {
		return mailmsg.DomainRelay{}, nil, refused
	}
	defer release()
	err = s.ssoStore.WithCurrentSettings(ctx, func(settings sso.SSOSettings) error {
		if !settings.Enabled || settings.IssuerURL != profile.Issuer {
			return refused
		}
		releaseRelay, err := fsutil.LockFileContext(ctx, path)
		if err != nil {
			return refused
		}
		defer releaseRelay()
		current, err := s.nativeDomains.ReadSet()
		if err != nil {
			return refused
		}
		for _, proof := range proofs {
			if !current.CurrentProof(proof) {
				return refused
			}
		}
		currentProfile, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
		if err != nil {
			return mailmsg.ErrDomainRelay
		}
		if !exists || !currentProfile.Equal(profile) || ctx.Err() != nil || sso.RequireNativeRestoreReleased(s.stateDir) != nil {
			return refused
		}
		return nil
	})
	if err != nil {
		return mailmsg.DomainRelay{}, nil, err
	}
	return profile, proofs, nil
}
