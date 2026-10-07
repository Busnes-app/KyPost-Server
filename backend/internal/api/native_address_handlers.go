package api

import (
	"errors"
	"net/http"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// handleNativeMailAddresses lists mailboxes and their addresses (GET, with an
// optional ?user=<id> filter) or adds an alias (POST {mailbox, address}).
func (s *Server) handleNativeMailAddresses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		mailboxes, err := s.ssoLifecycle.NativeMailboxes()
		if nativeMigrationRefused(w, err) {
			return
		}
		if err != nil {
			http.Error(w, "mail address state unreadable; preserve configuration and restore it", http.StatusServiceUnavailable)
			return
		}
		if user := r.URL.Query().Get("user"); user != "" {
			if _, err := s.users.Get(user); errors.Is(err, users.ErrNotFound) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			} else if err != nil {
				writeUserStoreError(w, err)
				return
			}
			owned := []sso.NativeMailbox{}
			for _, m := range mailboxes {
				if m.User == user {
					owned = append(owned, m)
				}
			}
			mailboxes = owned
		}
		writeJSON(w, http.StatusOK, map[string]any{"mailboxes": mailboxes})
		return
	}
	var body struct {
		Mailbox string `json:"mailbox"`
		Address string `json:"address"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	x, err := s.ssoLifecycle.AddNativeAlias(r.Context(), s.stateDir, body.Mailbox, body.Address)
	s.answerNativeAddress(w, r, "add_alias", x, err)
}

// handleNativeMailAddressRelease reserves an alias; the record is never deleted.
func (s *Server) handleNativeMailAddressRelease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct{}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	x, err := s.ssoLifecycle.ReleaseNativeAlias(r.Context(), s.stateDir, r.PathValue("address"))
	s.answerNativeAddress(w, r, "release_alias", x, err)
}

func (s *Server) handleNativeMailAddressReassign(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Mailbox string `json:"mailbox"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	x, err := s.ssoLifecycle.ReassignNativeAddress(r.Context(), s.stateDir, r.PathValue("address"), body.Mailbox)
	s.answerNativeAddress(w, r, "reassign_alias", x, err)
}

// answerNativeAddress maps the ledger's fixed errors and audits the outcome.
// The audit names the mailbox, never the address.
func (s *Server) answerNativeAddress(w http.ResponseWriter, r *http.Request, action string, x sso.NativeAddress, err error) {
	ac, _ := authFromContext(r)
	result := "committed"
	defer func() {
		s.logger.Info("native mail address change", "actor", ac.UserID, "action", action, "target", x.Mailbox, "result", result)
	}()
	if err == nil {
		writeJSON(w, http.StatusOK, x)
		return
	}
	result = "refused"
	if nativeMigrationRefused(w, err) {
		return
	}
	switch {
	case errors.Is(err, sso.ErrNativeAddressInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, sso.ErrNativeAddressUnknown):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, sso.ErrNativeAddressConflict), errors.Is(err, sso.ErrNativeAddressAdministrator), errors.Is(err, sso.ErrNativeAddressDomain), errors.Is(err, sso.ErrNativeRestoreHold):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "mail address change refused; storage must be readable and the receiving store consistent", http.StatusServiceUnavailable)
	}
}
