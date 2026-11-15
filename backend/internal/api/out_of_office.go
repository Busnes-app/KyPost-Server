package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// errOOOInput marks a refusal the caller can fix (400).
var errOOOInput = errors.New("invalid out-of-office settings")

type oooInputError struct{ err error }

func (e oooInputError) Error() string { return e.err.Error() }
func (e oooInputError) Unwrap() error { return errOOOInput }

// outOfOfficeView is the wire form (docs/OUT_OF_OFFICE.md).
type outOfOfficeView struct {
	config.OutOfOffice
	Active        bool `json:"active"`
	RemoteAllowed bool `json:"remoteAllowed"`
}

func (s *Server) oooRemoteAllowed() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.OutOfOfficeRemote
}

func (s *Server) oooView(o config.OutOfOffice) outOfOfficeView {
	o.Remote = o.RemoteMode()
	return outOfOfficeView{OutOfOffice: o, Active: o.ActiveAt(time.Now()), RemoteAllowed: s.oooRemoteAllowed()}
}

// loadOutOfOffice reads the user's setting.
func (s *Server) loadOutOfOffice(userID string) (config.OutOfOffice, error) {
	st, err := config.LoadUserSettings(s.userSettingsPath(userID))
	return st.OutOfOffice, err
}

// saveOutOfOffice applies change to the stored setting under the settings
// lock and validates the result.
func (s *Server) saveOutOfOffice(userID string, change func(prev config.OutOfOffice) (config.OutOfOffice, error)) (config.OutOfOffice, error) {
	var saved config.OutOfOffice
	err := config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error {
		next, err := change(st.OutOfOffice)
		if err != nil {
			return oooInputError{err}
		}
		if next, err = config.NormalizeOutOfOffice(st.OutOfOffice, next, s.oooRemoteAllowed()); err != nil {
			return oooInputError{err}
		}
		st.OutOfOffice, saved = next, next
		return nil
	})
	return saved, err
}

// handleOutOfOffice reads (session or paired device) and replaces (session)
// the caller's out-of-office reply. A dedicated administrator identity has no
// mailbox to reply from.
func (s *Server) handleOutOfOffice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if s.nativeMail {
		u, err := s.users.Get(ac.UserID)
		if err != nil {
			writeUserStoreError(w, err)
			return
		}
		if _, native, err := s.nativeMailAssignment(r.Context(), u.ID); u.Role == users.RoleAdmin && (!native || err != nil) {
			s.refuseNativeAdministrator(w, r, u.ID, sso.ErrNativeAdministrator)
			return
		}
	}
	if r.Method == http.MethodGet {
		o, err := s.loadOutOfOffice(ac.UserID)
		if err != nil {
			http.Error(w, "failed to read out-of-office settings", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, s.oooView(o))
		return
	}
	var req config.OutOfOffice
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid out-of-office payload"})
		return
	}
	saved, err := s.saveOutOfOffice(ac.UserID, func(config.OutOfOffice) (config.OutOfOffice, error) { return req, nil })
	switch {
	case errors.Is(err, errOOOInput):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case err != nil:
		http.Error(w, "failed to save out-of-office settings", http.StatusInternalServerError)
		return
	}
	s.logger.Info("out-of-office settings saved", "user_id", ac.UserID)
	writeJSON(w, http.StatusOK, s.oooView(saved))
}
