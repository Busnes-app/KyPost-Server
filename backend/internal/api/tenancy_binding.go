package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Identity binding in a multi-tenant install (docs/superpowers/specs/2026-11-20-tenancy-design.md,
// "Identity binding" and "PR 2 as built"). Every caller checks multiTenant first, so a
// single-tenant install never parses or compares an org_id.

func (s *Server) multiTenant() bool { return s.tenancy != nil }

var errTokenOrg = errors.New("the identity token has no valid org_id, or is export-only where that is refused")

// tokenOrg is a verified ID token's org and whether it carries the export_only claim (K
// open question 4 (a)). A missing, malformed or non-canonical org_id is refused.
func tokenOrg(c *sso.SSOTokenClaims) (org string, exportOnly bool, err error) {
	org, exportOnly = c.TenantClaims()
	if !config.ValidOrgID(org) {
		return "", false, errTokenOrg
	}
	return org, exportOnly, nil
}

// ssoAppAdmin is an SSO session's administrator ceiling. In multi mode it is K7: the
// token's own kypost.admin role, with no group fallback, and the staff org.
func (s *Server) ssoAppAdmin(c *sso.SSOTokenClaims, settings sso.SSOSettings, org string) bool {
	if !s.multiTenant() {
		return c.AppAdmin(settings.AdminGroup)
	}
	return c.Roles != nil && sso.HasAdminRole(c.Roles) && org == s.staffOrg
}

// checkAccountOrg proves an account is bound to org in all three records: users.json,
// user_orgs and subject_orgs. A user_orgs row a crash left unwritten is written here
// (K's step 3 on retry). Any disagreement is tenancy.ErrOrgMismatch.
func (s *Server) checkAccountOrg(ctx context.Context, u users.User, org string) error {
	if u.OrgID != org {
		return tenancy.ErrOrgMismatch
	}
	subject, err := tenancy.SubjectOrg(ctx, s.tenancy, u.SSOIssuer, u.SSOSub)
	if err != nil {
		return err
	}
	if subject != org {
		return tenancy.ErrOrgMismatch
	}
	if users.TableMode(s.configDir) {
		return tenancy.BindUserAccount(ctx, s.tenancy, s.stateDir, u.ID, org, s.staffOrg)
	}
	return tenancy.BindUser(ctx, s.tenancy, u.ID, org)
}

// orgRefused logs a binding refusal as a security event. It names no org, subject or
// address: only the path and, when known, the internal account ID.
func (s *Server) orgRefused(path, userID string) {
	s.logger.Warn("security: org binding refused", "action", path, "result", "refused", "actor", userID)
}

// subjectOrg is the org a new account is written with: the subject's subject_orgs row,
// which the signed event wrote first (step 1). "" in single mode.
func (s *Server) subjectOrg(ctx context.Context, issuer, subject string) (string, error) {
	if !s.multiTenant() {
		return "", nil
	}
	org, err := tenancy.SubjectOrg(ctx, s.tenancy, issuer, subject)
	if err == nil && org == "" {
		err = sso.ErrNativeProvisioning
	}
	return org, err
}

// bindNewAccount writes user_orgs for an account just written to users.json (step 3).
func (s *Server) bindNewAccount(ctx context.Context, u users.User) error {
	if !s.multiTenant() {
		return nil
	}
	if users.TableMode(s.configDir) {
		return tenancy.BindUserAccount(ctx, s.tenancy, s.stateDir, u.ID, u.OrgID, s.staffOrg)
	}
	return tenancy.BindUser(ctx, s.tenancy, u.ID, u.OrgID)
}

// bindDirectoryOrg runs before the revision fence for a signed directory event in multi
// mode. It refuses an event without a valid org_id, binds the subject's org once, refuses
// an event for another org than the subject's or account's, and applies K7 to u: a
// customer naming kypost.admin is applied as a member, and a staff-org subject without it
// is applied as inactive, because staff accounts exist only as administrators.
func (s *Server) bindDirectoryOrg(ctx context.Context, issuer, eventID string, body []byte, u *sso.DirectoryUser) (string, int, error) {
	var ev struct {
		OrgID string `json:"org_id"`
	}
	if json.Unmarshal(body, &ev) != nil || !config.ValidOrgID(ev.OrgID) {
		return "", http.StatusBadRequest, errors.New("invalid directory event: org_id is required")
	}
	refused := func(userID string) (string, int, error) {
		s.orgRefused("directory_event", userID)
		s.logger.Info("directory event refused", "event_id", eventID, "reason", "org binding mismatch")
		return "", http.StatusUnprocessableEntity, errors.New("directory event refused: the subject is bound to another org")
	}
	// The account is compared before the subject is bound, so an event for another org
	// never writes a binding.
	existing, err := s.users.GetBySSOSubIssuer(issuer, u.ID)
	found := err == nil
	switch {
	case found && existing.OrgID != ev.OrgID:
		return refused(existing.ID)
	case errors.Is(err, users.ErrNativeAccountConflict):
		return "", http.StatusUnprocessableEntity, errIssuerConflict
	case !found && !errors.Is(err, users.ErrNotFound):
		return "", http.StatusInternalServerError, errors.New("failed to look up user")
	}
	if err := tenancy.BindSubject(ctx, s.tenancy, issuer, u.ID, ev.OrgID); errors.Is(err, tenancy.ErrOrgMismatch) {
		return refused(existing.ID)
	} else if err != nil {
		return "", http.StatusInternalServerError, errors.New("failed to bind the subject's org")
	}
	if found {
		if err := s.checkAccountOrg(ctx, existing, ev.OrgID); errors.Is(err, tenancy.ErrOrgMismatch) {
			return refused(existing.ID)
		} else if err != nil {
			return "", http.StatusInternalServerError, errors.New("failed to check the account's org")
		}
	}
	// A deleted org's subject, or a purged one, never gets an account again (K "Nothing is
	// reused"): an active event applies as inactive, so removals still succeed.
	gone, err := tenancy.Purged(ctx, s.tenancy, ev.OrgID, u.ID)
	if err == nil && !gone && ev.OrgID != s.staffOrg {
		var o tenancy.Org
		o, err = tenancy.LookupOrg(ctx, s.tenancy, ev.OrgID)
		gone = o.Status == suitedir.OrgDeleted
	}
	if err != nil {
		return "", http.StatusInternalServerError, errors.New("failed to read the org's state")
	}
	if gone {
		inactive := false
		u.Active = &inactive
		s.logger.Info("directory event for a deleted org applied as inactive", "event_id", eventID)
	}
	switch admin := sso.HasAdminRole(u.Roles); {
	case admin && ev.OrgID != s.staffOrg:
		u.Roles = json.RawMessage(`[]`)
		s.logger.Warn("security: customer directory event named the administrator role; applied as a member", "event_id", eventID)
	case !admin && ev.OrgID == s.staffOrg && u.Active != nil && *u.Active:
		inactive := false
		u.Active = &inactive
	}
	return ev.OrgID, 0, nil
}
