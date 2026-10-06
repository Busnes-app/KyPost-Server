package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
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
		// Neither relay authentication username nor password is a read API field.
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": exists, "domain": c.Domain, "issuer": c.Issuer,
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
		Host         string `json:"host"`
		Port         int    `json:"port"`
		SMTPUsername string `json:"smtpUsername"`
		SMTPPassword string `json:"smtpPassword"`
		Password     string `json:"password"`
		AuthSecret   string `json:"authSecret"`
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	proof, err := s.nativeDomains.Verify(ctx) // DNS precedes all disk fences.
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil || proof.Issuer != settings.IssuerURL {
		http.Error(w, "relay setup requires fresh issuer-bound domain proof; verify the current TXT record", http.StatusConflict)
		return
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, sso.NativeDomainsFile))
	if err != nil {
		http.Error(w, "relay setup busy or cancelled; retry without deleting configuration", http.StatusServiceUnavailable)
		return
	}
	defer release()
	current, err := s.nativeDomains.Read()
	currentSettings := s.ssoStore.Load()
	if err != nil || current != proof || proof.VerifiedUntil <= time.Now().Unix() || !currentSettings.Enabled || currentSettings.IssuerURL != proof.Issuer {
		http.Error(w, "mail-domain authority changed during relay setup; reverify and retry", http.StatusConflict)
		return
	}
	if body.Port == 0 {
		body.Port = 465
	}
	if err := sso.RequireNativeRestoreReleased(s.stateDir); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	c, err := mailmsg.SaveDomainRelay(ctx, path, keyPath, mailmsg.DomainRelay{
		Domain: proof.Domain, Issuer: proof.Issuer, Host: body.Host, Port: body.Port, Username: body.SMTPUsername, Password: body.SMTPPassword,
	})
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
	profile, proof, err := s.nativeRelayCheckProfile(ctx, body.ExpectedGeneration, nil)
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
	if _, _, err := s.nativeRelayCheckProfile(ctx, body.ExpectedGeneration, &proof); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generation": profile.Generation, "host": profile.Host, "port": profile.Port,
		"tls": true, "authenticated": true, "deliveryTested": false,
	})
}

// DNS runs before domain -> settings -> relay disk fences. After network I/O,
// stable challenge identity must still match even though Verify renews its cache.
func (s *Server) nativeRelayCheckProfile(ctx context.Context, generation string, prior *sso.NativeDomain) (mailmsg.DomainRelay, sso.NativeDomain, error) {
	refused := errors.New("relay check authority or saved profile changed; verify the current TXT record, issuer and restore status, then reload the saved relay")
	path, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	profile, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
	if err != nil {
		return mailmsg.DomainRelay{}, sso.NativeDomain{}, mailmsg.ErrDomainRelay
	}
	if !exists || profile.Generation != generation || sso.RequireNativeRestoreReleased(s.stateDir) != nil {
		return mailmsg.DomainRelay{}, sso.NativeDomain{}, refused
	}
	proof, err := s.nativeDomains.Verify(ctx)
	if err != nil || !proof.Established || proof.Domain != profile.Domain || proof.Issuer != profile.Issuer ||
		prior != nil && (proof.Domain != prior.Domain || proof.Issuer != prior.Issuer || proof.Token != prior.Token || proof.ExpiresAt != prior.ExpiresAt) {
		return mailmsg.DomainRelay{}, sso.NativeDomain{}, refused
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, sso.NativeDomainsFile))
	if err != nil {
		return mailmsg.DomainRelay{}, sso.NativeDomain{}, refused
	}
	defer release()
	err = s.ssoStore.WithCurrentSettings(ctx, func(settings sso.SSOSettings) error {
		if !settings.Enabled || settings.IssuerURL != proof.Issuer {
			return refused
		}
		releaseRelay, err := fsutil.LockFileContext(ctx, path)
		if err != nil {
			return refused
		}
		defer releaseRelay()
		current, err := s.nativeDomains.Read()
		if err != nil || current != proof || proof.VerifiedUntil <= time.Now().Unix() {
			return refused
		}
		currentProfile, exists, err := mailmsg.ReadDomainRelay(path, keyPath)
		if err != nil {
			return mailmsg.ErrDomainRelay
		}
		if !exists || currentProfile != profile || ctx.Err() != nil || sso.RequireNativeRestoreReleased(s.stateDir) != nil {
			return refused
		}
		return nil
	})
	if err != nil {
		return mailmsg.DomainRelay{}, sso.NativeDomain{}, err
	}
	return profile, proof, nil
}
