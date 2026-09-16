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

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// Admin assignment of another user's mailbox. Writes the same encrypted file
// the user's own route writes, so the poller and every reader pick it up
// unchanged. Managed=true additionally locks the user's own route (Task 1).

// sameIMAPDestination reports whether a and b would connect to the same
// mailbox, ignoring credentials. A blank password may only be reused from
// storage when this holds — otherwise an admin could repoint a user's
// stored secret at a destination of the admin's choosing.
func sameIMAPDestination(a, b imapConfigPayload) bool {
	return a.Host == b.Host && a.Port == b.Port && a.SMTPHost == b.SMTPHost &&
		a.SMTPPort == b.SMTPPort && a.Username == b.Username
}

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
		// lock without knowing the secret — but only when the destination is
		// unchanged, so a blank password can never repoint a stored secret.
		if payload.Password == "" && exists {
			if !sameIMAPDestination(payload, stored) {
				http.Error(w, "a new destination requires a new password", http.StatusBadRequest)
				return
			}
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

// sameCardDAVDestination mirrors sameIMAPDestination for the CardDAV client
// config: a blank password may only be reused from storage when the
// destination it would authenticate against is unchanged.
func sameCardDAVDestination(a, b carddavClientConfigPayload) bool {
	return a.ServerURL == b.ServerURL && a.Username == b.Username
}

func (s *Server) handleAdminUserCardDAVClient(w http.ResponseWriter, r *http.Request) {
	ac, _ := authFromContext(r)
	target, err := s.users.Get(r.PathValue("id"))
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	path := s.userCardDAVClientConfigPath(target.ID)
	switch r.Method {
	case http.MethodGet:
		payload, exists, err := readCardDAVClientConfigPayload(path, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read carddav client configuration", http.StatusInternalServerError)
			return
		}
		if !exists {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		writeJSON(w, http.StatusOK, cardDAVClientStatusResponse(payload))
	case http.MethodPut:
		var payload carddavClientConfigPayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		payload = normalizeCardDAVClientPayload(payload)
		stored, exists, err := readCardDAVClientConfigPayload(path, s.imapConfigKeyPath)
		if err != nil {
			http.Error(w, "failed to read carddav client configuration", http.StatusInternalServerError)
			return
		}
		if payload.Password == "" && exists {
			if !sameCardDAVDestination(payload, stored) {
				http.Error(w, "a new destination requires a new password", http.StatusBadRequest)
				return
			}
			payload.Password = stored.Password
		}
		// Carry forward informational sync state: this route only ever changes
		// credentials, the server URL, or the managed lock, and must not reset
		// history an unattended sync already recorded.
		if exists {
			payload.LastSyncedAt = stored.LastSyncedAt
			payload.LastSyncError = stored.LastSyncError
			payload.LastSyncImported = stored.LastSyncImported
			payload.LastSyncUpdated = stored.LastSyncUpdated
			payload.DiscoveredAddressBooks = stored.DiscoveredAddressBooks
			if payload.AddressBookPath == "" {
				payload.AddressBookPath = stored.AddressBookPath
			}
		}
		if payload.ServerURL == "" || payload.Username == "" || payload.Password == "" {
			http.Error(w, "serverUrl, username, and password are required", http.StatusBadRequest)
			return
		}
		if err := rejectURLUserinfo(payload.ServerURL); err != nil {
			http.Error(w, "serverUrl must not embed credentials", http.StatusBadRequest)
			return
		}
		// Same https-only SSRF guard as the user route; see carddav_client.go.
		if err := validateOutboundURL(payload.ServerURL, outboundCardDAVSchemes...); err != nil {
			s.logger.Info("carddav server url refused", "user_id", target.ID, "admin_id", ac.UserID, "error", err.Error())
			http.Error(w, "serverUrl is not reachable", http.StatusBadRequest)
			return
		}
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			http.Error(w, "failed to prepare config directory", http.StatusInternalServerError)
			return
		}
		if err := writeCardDAVClientConfigPayload(path, s.imapConfigKeyPath, payload); err != nil {
			http.Error(w, "failed to save carddav client configuration", http.StatusInternalServerError)
			return
		}
		s.logger.Info("user carddav client assigned by admin", "user_id", target.ID, "admin_id", ac.UserID, "managed", strconv.FormatBool(payload.Managed))
		writeJSON(w, http.StatusOK, cardDAVClientStatusResponse(payload))
	case http.MethodDelete:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "failed to remove carddav client configuration", http.StatusInternalServerError)
			return
		}
		s.logger.Info("user carddav client removed by admin", "user_id", target.ID, "admin_id", ac.UserID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
