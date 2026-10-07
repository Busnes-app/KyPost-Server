package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// mailboxHeader selects one of the caller's mailboxes on mail endpoints.
const mailboxHeader = "X-KyPost-Mailbox"

// withMailbox admits the mailbox an X-KyPost-Mailbox header selects, inside
// withMailAuth, on every request. Absent or the caller's own user ID is the
// primary, so older clients are unchanged. Unknown, foreign and disabled
// mailboxes get the same 404 before any storage is opened: the header is
// resolved through the ledger and never used as a path before that.
func (s *Server) withMailbox(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ac, ok := authFromContext(r)
		id := r.Header.Get(mailboxHeader)
		if !ok || id == "" || id == ac.UserID {
			next(w, r)
			return
		}
		if _, _, err := s.nativeMailboxAssignment(r.Context(), ac.UserID, id); err != nil {
			if errors.Is(err, sso.ErrNativeMailboxUnknown) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "mailbox not found"})
				return
			}
			http.Error(w, "mailbox authority is unavailable", http.StatusServiceUnavailable)
			return
		}
		ac.Mailbox = id
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, ac)))
	}
}

// selectedExtraMailbox reports whether ctx carries an admitted extra mailbox.
func selectedExtraMailbox(ctx context.Context) bool {
	ac, _ := ctx.Value(authContextKey{}).(AuthContext)
	return ac.Mailbox != ""
}

// extraMailboxIDs lists the user's prepared extra mailboxes in any state, for
// maintenance of their caches; none outside native mode. An unprepared one
// has no directory, and none may be created for it before preparation.
func (s *Server) extraMailboxIDs(userID string) []string {
	if !s.nativeMail {
		return nil
	}
	all, err := s.ssoLifecycle.NativeMailboxes()
	if err != nil {
		s.logger.Error("cannot list mailboxes", "user_id", userID, "error", err.Error())
		return nil
	}
	ids := []string{}
	for _, m := range all {
		if m.User != userID || m.Kind != "extra" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.mailboxStateDir(userID, m.ID), "native-mailbox.json")); err == nil {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

type clientMailbox struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Addresses []clientAddress `json:"addresses"`
}

type clientAddress struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
}

// handleMailboxes lists the caller's accessible mailboxes (primary first) and
// their active addresses; disabled mailboxes are omitted. It ignores the
// selection header. A non-native account has none.
func (s *Server) handleMailboxes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	out := []clientMailbox{}
	if _, native, err := s.nativeMailAssignment(r.Context(), ac.UserID); err != nil || !native {
		if err != nil {
			http.Error(w, "mailbox authority is unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mailboxes": out})
		return
	}
	all, err := s.ssoLifecycle.NativeMailboxes()
	if err != nil {
		http.Error(w, "mailbox authority is unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, m := range all {
		// The primary was admitted above; its ledger state may lag a
		// reactivation until the worker reconciles it.
		if m.User != ac.UserID || m.Kind == "extra" && m.State != "active" {
			continue
		}
		box := clientMailbox{ID: m.ID, Kind: m.Kind, Addresses: []clientAddress{}}
		for _, x := range m.Addresses {
			if x.State == "active" {
				box.Addresses = append(box.Addresses, clientAddress{Address: x.Address, Kind: x.Kind})
			}
		}
		if m.Kind == "primary" {
			out = append([]clientMailbox{box}, out...)
		} else {
			out = append(out, box)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": out})
}

// handleNativeMailboxes lists mailboxes (GET, optional ?user=<id>) or creates
// an extra mailbox (POST {user, address}) for an everyday identity.
func (s *Server) handleNativeMailboxes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleNativeMailAddresses(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		User    string `json:"user"`
		Address string `json:"address"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	// Only a published native account gets an extra mailbox.
	u, err := s.users.Get(body.User)
	switch {
	case err == nil && u.NativeMailboxSource == "" && u.Role == users.RoleAdmin:
		err = sso.ErrNativeAddressAdministrator
	case errors.Is(err, users.ErrNotFound) || err == nil && u.NativeMailboxSource == "":
		err = sso.ErrNativeAddressUnknown
	}
	m := sso.NativeMailbox{}
	if err == nil {
		m, err = s.ssoLifecycle.CreateNativeMailbox(r.Context(), s.stateDir, body.User, body.Address)
	}
	s.answerNativeMailbox(w, r, "create_mailbox", m, err)
}

func (s *Server) handleNativeMailboxDisable(w http.ResponseWriter, r *http.Request) {
	s.setNativeMailboxState(w, r, "disable_mailbox", false)
}

func (s *Server) handleNativeMailboxEnable(w http.ResponseWriter, r *http.Request) {
	s.setNativeMailboxState(w, r, "enable_mailbox", true)
}

func (s *Server) setNativeMailboxState(w http.ResponseWriter, r *http.Request, action string, active bool) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct{}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	m, err := s.ssoLifecycle.SetNativeMailboxState(r.Context(), s.stateDir, r.PathValue("id"), active)
	if m.ID == "" {
		m.ID = r.PathValue("id")
	}
	s.answerNativeMailbox(w, r, action, m, err)
}

// answerNativeMailbox maps the ledger's fixed errors as answerNativeAddress
// does and audits the mailbox, never an address.
func (s *Server) answerNativeMailbox(w http.ResponseWriter, r *http.Request, action string, m sso.NativeMailbox, err error) {
	if err == nil {
		ac, _ := authFromContext(r)
		s.logger.Info("native mailbox change", "actor", ac.UserID, "action", action, "target", m.ID, "result", "committed")
		writeJSON(w, http.StatusOK, m)
		return
	}
	if errors.Is(err, sso.ErrNativeRoutesPending) {
		ac, _ := authFromContext(r)
		s.logger.Info("native mailbox change", "actor", ac.UserID, "action", action, "target", m.ID, "result", "committed_routes_pending")
		writeJSON(w, http.StatusOK, map[string]any{"mailbox": m.ID, "user": m.User, "kind": m.Kind, "state": m.State, "addresses": m.Addresses, "warning": sso.ErrNativeRoutesPending.Error()})
		return
	}
	s.answerNativeAddress(w, r, action, m.ID, sso.NativeAddress{}, err)
}
