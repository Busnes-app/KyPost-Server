package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	imapadapter "github.com/Busness-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busness-app/kypost-server/backend/internal/mailmsg"
)

// Admin assignment of another user's mailbox. Writes the same encrypted file
// the user's own route writes, so the poller and every reader pick it up
// unchanged. Managed=true additionally locks the user's own route (Task 1).

func (s *Server) handleAdminUserIMAPConfig(w http.ResponseWriter, r *http.Request) {
	ac, _ := authFromContext(r)
	target, err := s.users.Get(r.PathValue("id"))
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	path := s.userIMAPConfigPath(target.ID)
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := mailmsg.ReadIMAPConfigPayload(path, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read imap configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		writeJSON(w, http.StatusOK, imapConfigStatus(path, s.imapConfigKeyPath, payload))
	case http.MethodPut:
		var payload imapConfigPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		payload = mailmsg.NormalizeIMAPPayload(payload)
		stored, exists, err := mailmsg.ReadIMAPConfigPayload(path, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read imap configuration", http.StatusInternalServerError)
			return
		}
		// Blank password means "keep what is stored", so an admin can flip the
		// lock or fix a host without knowing the secret.
		if payload.Password == "" && exists {
			payload.Password = stored.Password
		}
		if payload.Host == "" || payload.Username == "" || payload.Password == "" {
			http.Error(w, "host, username, and password are required", http.StatusBadRequest)
			return
		}
		if err := imapadapter.ValidateMailboxName(payload.Mailbox); err != nil {
			http.Error(w, "invalid mailbox name", http.StatusBadRequest)
			return
		}
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			http.Error(w, "failed to prepare config directory", http.StatusInternalServerError)
			return
		}
		if err := writeIMAPConfigPayload(path, s.imapConfigKeyPath, payload); err != nil {
			http.Error(w, "failed to save imap configuration", http.StatusInternalServerError)
			return
		}
		s.invalidateUserMail(target.ID)
		s.logger.Info("user mailbox assigned by admin", "user_id", target.ID, "admin_id", ac.UserID, "managed", strconv.FormatBool(payload.Managed), "host", payload.Host)
		status := imapConfigStatus(path, s.imapConfigKeyPath, payload)
		status["ok"] = true
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove imap configuration", http.StatusInternalServerError)
			return
		}
		s.invalidateUserMail(target.ID)
		s.logger.Info("user mailbox removed by admin", "user_id", target.ID, "admin_id", ac.UserID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
