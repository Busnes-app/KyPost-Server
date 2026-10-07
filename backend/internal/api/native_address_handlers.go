package api

import (
	"errors"
	"net/http"
	"strings"

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
	s.answerNativeAddress(w, r, "add_alias", body.Mailbox, x, err)
}

// handleNativeMailAddressRelease reserves an alias; the record is never deleted.
func (s *Server) handleNativeMailAddressRelease(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct{}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	// The audit names the mailbox that held the address, never the address.
	held := ""
	if addresses, err := s.ssoLifecycle.NativeAddresses(); err == nil {
		held = addresses[strings.ToLower(r.PathValue("address"))].Mailbox
	}
	x, err := s.ssoLifecycle.ReleaseNativeAlias(r.Context(), s.stateDir, r.PathValue("address"))
	s.answerNativeAddress(w, r, "release_alias", held, x, err)
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
	s.answerNativeAddress(w, r, "reassign_alias", body.Mailbox, x, err)
}

// answerNativeAddress maps the ledger's fixed errors and audits the outcome
// against target, the requested or holding mailbox; never the address. A
// committed change whose route write is still pending is a success with a
// warning: the ledger already decides routing.
func (s *Server) answerNativeAddress(w http.ResponseWriter, r *http.Request, action, target string, x sso.NativeAddress, err error) {
	ac, _ := authFromContext(r)
	result := "committed"
	defer func() {
		s.logger.Info("native mail address change", "actor", ac.UserID, "action", action, "target", target, "result", result)
	}()
	if err == nil {
		writeJSON(w, http.StatusOK, x)
		return
	}
	if errors.Is(err, sso.ErrNativeRoutesPending) {
		result = "committed_routes_pending"
		writeJSON(w, http.StatusOK, map[string]any{"address": x.Address, "mailbox": x.Mailbox, "kind": x.Kind, "state": x.State, "generation": x.Generation, "warning": sso.ErrNativeRoutesPending.Error()})
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
	case errors.Is(err, sso.ErrNativeAddressConflict), errors.Is(err, sso.ErrNativeAddressAdministrator), errors.Is(err, sso.ErrNativeAddressDomain), errors.Is(err, sso.ErrNativeAddressDirectory), errors.Is(err, sso.ErrNativeRestoreHold):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "mail address change refused; storage must be readable and the receiving store consistent", http.StatusServiceUnavailable)
	}
}
