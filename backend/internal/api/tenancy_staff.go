package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The staff console (tenancy design "Admin surface", build plan PR 4). Every administrator
// route in server.go names exactly one class wrapper below; a bare withAdmin route fails
// TestEveryAdminRouteHasAClass. Single mode: every class is withAdmin, unchanged.

// orgHeader is the staff org selection on an org-bound route (T "Org selection").
const orgHeader = "X-Ky-Org"

type staffOrgKey struct{}

// withPlatformAdmin is an install-wide (platform) route: staff only in multi mode.
func (s *Server) withPlatformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAdmin(next)
}

// withRefusedAdmin is a route K6 turns off: 404 in multi mode, after the administrator
// check, so a customer member still gets today's 403.
func (s *Server) withRefusedAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAdmin(s.offInMulti(next))
}

// withOrgAdmin is an org-bound route: in multi mode staff name one org in X-Ky-Org, read
// only after withAdmin has proved K7. The org must be the staff org or in the orgs cache.
// Each request writes staff.read or staff.write to the audit log, naming the org and the
// route pattern only, before the handler runs; a request that cannot be audited is refused.
func (s *Server) withOrgAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		if !s.multiTenant() {
			next(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		v := r.Header.Values(orgHeader)
		switch {
		case len(v) == 0 || len(v) == 1 && v[0] == "":
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "org_required"})
			return
		case len(v) != 1 || !config.ValidOrgID(v[0]):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "org_invalid"})
			return
		}
		org := v[0]
		if org != s.staffOrg {
			known, err := tenancy.KnownOrg(r.Context(), s.tenancy, org)
			if err != nil {
				writeUnavailable(w)
				return
			}
			if !known {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "org_unknown"})
				return
			}
		}
		action := "staff.write"
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			action = "staff.read"
		}
		ac, _ := authFromContext(r)
		if s.backup == nil || s.backup.Audit(action, ac.UserID, "org:"+org, "requested", map[string]any{"route": r.Pattern}) != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the audit log is unavailable; nothing was changed"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), staffOrgKey{}, org)))
	})
}

// offInMulti answers 404 in multi mode (K6), as an unknown route does.
func (s *Server) offInMulti(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.multiTenant() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// staffOrgAdmit is the org gate of an org-bound admin route: in multi mode u must belong to
// the selected org. Reached without a selection it refuses.
func (s *Server) staffOrgAdmit(r *http.Request, u users.User) bool {
	if !s.multiTenant() {
		return true
	}
	org, _ := r.Context().Value(staffOrgKey{}).(string)
	return s.orgAdmit(AuthContext{OrgID: org}, u)
}

// staffTarget is nil when the account id names is in the selected org (always, in single
// mode). Another org's account is users.ErrNotFound, so the route answers as for a
// missing one; a store failure is returned as it is.
func (s *Server) staffTarget(r *http.Request, id string) error {
	if !s.multiTenant() {
		return nil
	}
	u, err := s.users.Get(id)
	if err == nil && !s.staffOrgAdmit(r, u) {
		err = users.ErrNotFound
	}
	return err
}

// staffMailboxAdmit reports whether the mailbox's account is in the selected org (always,
// in single mode). An unknown mailbox or account is false; only a read failure is an error.
func (s *Server) staffMailboxAdmit(r *http.Request, mailboxID string) (bool, error) {
	if !s.multiTenant() {
		return true, nil
	}
	a, ok, err := s.ssoLifecycle.NativeMailboxAssignment(mailboxID)
	if err != nil || !ok {
		return false, err
	}
	u, err := s.users.Get(a.UserID())
	if errors.Is(err, users.ErrNotFound) {
		return false, nil
	}
	return err == nil && s.staffOrgAdmit(r, u), err
}

// staffAddressAdmit is staffMailboxAdmit for the mailbox that holds address.
func (s *Server) staffAddressAdmit(r *http.Request, address string) (bool, error) {
	if !s.multiTenant() {
		return true, nil
	}
	x, err := s.ssoLifecycle.NativeAddress(strings.ToLower(address))
	if err != nil || x.Address == "" {
		return false, err
	}
	return s.staffMailboxAdmit(r, x.Mailbox)
}

// staffOrgMailboxes keeps the mailboxes whose account is in the selected org (all, in
// single mode).
func (s *Server) staffOrgMailboxes(r *http.Request, mailboxes []sso.NativeMailbox) ([]sso.NativeMailbox, error) {
	if !s.multiTenant() {
		return mailboxes, nil
	}
	all, err := s.users.List()
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for _, u := range all {
		in[u.ID] = s.staffOrgAdmit(r, u)
	}
	return slices.DeleteFunc(mailboxes, func(m sso.NativeMailbox) bool { return !in[m.User] }), nil
}

// isStaff is K7 on the request side: in multi mode an administrator must also be in the
// staff org. PR 2's users-store rules already refuse a customer administrator; this
// checks again where every admin route is admitted.
func (s *Server) isStaff(ac AuthContext) bool {
	return !s.multiTenant() || ac.OrgID != "" && ac.OrgID == s.staffOrg
}
