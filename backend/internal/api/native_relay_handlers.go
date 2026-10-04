package api

import (
	"context"
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
			"transport": "implicit-tls", "authRequired": true, "sendingEnabled": false,
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
	if err != nil || proof.Issuer != settings.IssuerURL {
		http.Error(w, "relay setup requires fresh issuer-bound domain proof; verify the current TXT record", http.StatusConflict)
		return
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(s.configDir, "native-domain.json"))
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
