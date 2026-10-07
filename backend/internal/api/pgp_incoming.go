package api

import (
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func (s *Server) handlePGPIncoming(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	path := s.userSettingsPath(ac.UserID)
	switch r.Method {
	case http.MethodGet:
		settings, err := config.LoadUserSettings(path)
		if err != nil {
			http.Error(w, "cannot read incoming encryption preference", http.StatusInternalServerError)
			return
		}
		_, err = os.Stat(filepath.Join(s.userStateDir(ac.UserID), "incoming-encryption.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "cannot read incoming encryption status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": settings.EncryptIncoming, "pending": err == nil})
	case http.MethodPut:
		var req struct {
			Enabled                bool    `json:"enabled"`
			AcknowledgeReplacement bool    `json:"acknowledgeReplacement"`
			ExpectedRevision       *uint64 `json:"expectedRevision"`
			Password               string  `json:"password,omitempty"`
			AuthSecret             string  `json:"authSecret,omitempty"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid incoming encryption preference", http.StatusBadRequest)
			return
		}
		if req.Enabled {
			if owns, err := s.ownsActiveExtraMailbox(ac.UserID); err != nil {
				http.Error(w, "cannot verify your mailboxes; incoming encryption was not enabled", http.StatusServiceUnavailable)
				return
			} else if owns {
				writeJSON(w, http.StatusConflict, map[string]any{"error": errIncomingEncryptionExtraMailbox.Error()})
				return
			}
		}
		if !s.requirePGPStepUp(w, r, ac.UserID, req.Password, req.AuthSecret) {
			return
		}
		if req.Enabled {
			if !req.AcknowledgeReplacement {
				http.Error(w, "acknowledge message replacement and save a private-key recovery backup before enabling", http.StatusBadRequest)
				return
			}
			u, err := s.users.Get(ac.UserID)
			if err != nil {
				http.Error(w, "cannot read PGP identity", http.StatusInternalServerError)
				return
			}
			if req.ExpectedRevision == nil || *req.ExpectedRevision != u.PGPRevision {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "PGP state changed; reload before enabling", "pgpStateChanged": true})
				return
			}
			status, err := pgpmail.CheckKeyStatus(u.PGPPublicKey)
			if !u.Active || u.PGPProtection() != users.PGPProtectionClient || u.PGPPrivateKeyWrapped == "" || err != nil || !status.Usable() {
				http.Error(w, "set up a usable client-protected PGP key before enabling incoming encryption", http.StatusBadRequest)
				return
			}
			_, native, err := s.nativeMailAssignment(r.Context(), ac.UserID)
			if err != nil {
				http.Error(w, "cannot verify native mailbox authority; repair domain provisioning before enabling", http.StatusServiceUnavailable)
				return
			}
			if !native {
				mailbox, exists, err := mailmsg.ReadIMAPConfigPayload(s.userIMAPConfigPath(ac.UserID), s.imapConfigKeyPath)
				if err != nil {
					http.Error(w, "cannot verify polling mailbox; repair IMAP configuration before enabling", http.StatusInternalServerError)
					return
				}
				if exists && !strings.EqualFold(strings.TrimSpace(mailmsg.NormalizeIMAPPayload(mailbox).Mailbox), "INBOX") {
					http.Error(w, "incoming encryption requires the configured polling mailbox to be INBOX", http.StatusBadRequest)
					return
				}
			}
			cache, err := s.userMailCacheStore(ac.UserID)
			if err != nil {
				http.Error(w, "cannot open mail cache", http.StatusInternalServerError)
				return
			}
			// Commit the cache policy before the preference: a partial failure can only
			// leave fewer plaintext copies, never enable encryption with body caching.
			if err := cache.OmitBodies(); err != nil {
				http.Error(w, "cannot clear plaintext mail cache; encryption was not enabled", http.StatusInternalServerError)
				return
			}
		}
		if err := config.UpdateUserSettings(path, func(settings *config.UserSettings) error { settings.EncryptIncoming = req.Enabled; return nil }); err != nil {
			http.Error(w, "cannot save incoming encryption preference", http.StatusInternalServerError)
			return
		}
		s.logger.Info("incoming encryption preference updated", "user_id", ac.UserID, "detail", "enabled="+strconv.FormatBool(req.Enabled))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
