package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busness-app/kypost-server/backend/internal/fsutil"
)

// mailDefaults are instance-wide server settings an admin publishes so a new
// user's Email Settings form starts filled in. No credentials live here: every
// signed-in user can read it.
type mailDefaults struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	SMTPHost string `json:"smtpHost"`
	SMTPPort int    `json:"smtpPort"`
}

func (s *Server) mailDefaultsPath() string {
	return filepath.Join(s.configDir, "mail-defaults.json")
}

// loadMailDefaults returns the zero value when the file does not exist.
func (s *Server) loadMailDefaults() (mailDefaults, error) {
	var d mailDefaults
	raw, err := os.ReadFile(s.mailDefaultsPath())
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(raw, &d)
}

func (s *Server) handleMailDefaults(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d, err := s.loadMailDefaults()
		if err != nil {
			ac, _ := authFromContext(r)
			s.logger.Error("failed to read mail defaults", "user_id", ac.UserID, "error", err.Error())
			http.Error(w, "failed to read mail defaults", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, d)
	case http.MethodPut:
		var d mailDefaults
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&d); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		d.Host = strings.TrimSpace(d.Host)
		d.SMTPHost = strings.TrimSpace(d.SMTPHost)
		if d.Port < 0 || d.Port > 65535 || d.SMTPPort < 0 || d.SMTPPort > 65535 {
			http.Error(w, "port out of range", http.StatusBadRequest)
			return
		}
		raw, _ := json.MarshalIndent(d, "", "  ")
		if err := fsutil.AtomicWriteFile(s.mailDefaultsPath(), raw, 0o600); err != nil {
			http.Error(w, "failed to save mail defaults", http.StatusInternalServerError)
			return
		}
		ac, _ := authFromContext(r)
		s.logger.Info("mail defaults updated", "admin_id", ac.UserID, "host", d.Host, "smtp_host", d.SMTPHost)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": d.Host, "port": d.Port, "smtpHost": d.SMTPHost, "smtpPort": d.SMTPPort})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
