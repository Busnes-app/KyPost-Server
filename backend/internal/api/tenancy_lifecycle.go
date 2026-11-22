package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Entitlements and lifecycle (docs/superpowers/specs/2026-11-20-tenancy-design.md, K8,
// "Lifecycle" and "PR 5 as built"). A single-tenant install never polls and gates nothing.

const orgPollInterval = time.Minute

// reasonHeld is a scheduled send's reason while its org is not served; past the retry
// window it returns to Drafts with it (K open question 6).
const reasonHeld = "sending is paused for this account; it was not sent"

var errNoSuiteDirectory = errors.New("the suite directory is not configured")

// errOrgMailboxQuota refuses an extra mailbox past the org's mailboxes quota.
var errOrgMailboxQuota = errors.New("this organisation has reached its mailbox quota")

func (s *Server) orgGate() *tenancy.Gate {
	return &tenancy.Gate{DB: s.tenancy, StaffOrg: s.staffOrg}
}

// orgServe is nil when org's members may sign in, hold a credential and send now.
func (s *Server) orgServe(ctx context.Context, org string) error {
	if !s.multiTenant() {
		return nil
	}
	return s.orgGate().Serve(ctx, org)
}

// orgServed reports whether u's org is served; err is a cache read failure or a stale
// cache (tenancy.ErrStale), which a credential check answers as "try again later", never
// as a dead credential.
func (s *Server) orgServed(ctx context.Context, u users.User) (served bool, err error) {
	err = s.orgServe(ctx, u.OrgID)
	if errors.Is(err, tenancy.ErrNotServed) && !errors.Is(err, tenancy.ErrStale) {
		return false, nil
	}
	return err == nil, err
}

// StartOrgPoller refreshes the orgs cache about every minute, on the api-sweepers owner.
func (s *Server) StartOrgPoller(ctx context.Context) {
	if !s.multiTenant() {
		return
	}
	t := time.NewTicker(orgPollInterval)
	defer t.Stop()
	for {
		if err := s.pollOrgs(ctx, time.Now()); err != nil && ctx.Err() == nil {
			// The error can quote an org ID; the line names none.
			s.logger.Error("org entitlements not refreshed; members are refused once the cache is 15 minutes old", "error", "suite directory unavailable or its answer malformed")
		}
		// The purge reads only the cache, so it runs whether or not this poll succeeded. Its
		// errors can carry paths and database detail; the line names none.
		if err := s.runPurges(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.logger.Error("an org purge did not finish; it runs again after the next poll", "error", "a purge step or its final check failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOrgs applies one /api/suite/orgs answer, refusing the whole answer if any entry is
// malformed. An org that stops being served has its
// members' sessions revoked before its row changes, so a failed revocation is retried by
// the next poll. The poll stamp is written only after every org applied.
func (s *Server) pollOrgs(ctx context.Context, now time.Time) error {
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if !ok {
		return errNoSuiteDirectory
	}
	since, err := tenancy.OrgCursor(ctx, s.tenancy)
	if err != nil {
		return err
	}
	// A call cut short at its page bound returns a cursor below its last revision; keep
	// listing, so MarkPolled never stamps fresh an org whose change is still unread.
	for {
		orgs, next, err := dir.Orgs(ctx, since)
		if err != nil {
			return err
		}
		if err := s.applyOrgs(ctx, orgs, now); err != nil {
			return err
		}
		if len(orgs) == 0 || next >= orgs[len(orgs)-1].Revision {
			return tenancy.MarkPolled(ctx, s.tenancy, now)
		}
		since = next
	}
}

func (s *Server) applyOrgs(ctx context.Context, orgs []suitedir.Org, now time.Time) error {
	// A malformed entry fails the poll before any session is revoked.
	for _, o := range orgs {
		if err := tenancy.ValidOrg(o); err != nil {
			return err
		}
	}
	for _, o := range orgs {
		old, err := tenancy.LookupOrg(ctx, s.tenancy, o.ID)
		if err != nil {
			return err
		}
		if err := s.revokeIfStopping(ctx, old, o); err != nil {
			return err
		}
		if err := tenancy.ApplyOrg(ctx, s.tenancy, o, now); err != nil {
			return err
		}
	}
	return nil
}

// revokeIfStopping revokes every session of next's members when next stops an org that
// was served or unknown (K "Org status": R's session_revocations per user). The staff org
// is never gated, and a stale revision or a deleted row will not apply.
func (s *Server) revokeIfStopping(ctx context.Context, old tenancy.Org, next suitedir.Org) error {
	if next.ID == s.staffOrg || next.Status == suitedir.OrgActive && next.Enabled {
		return nil
	}
	// A row that is not served (deleted included) already refuses its members.
	served := old.Status == suitedir.OrgActive && old.Enabled
	if old.Known && (next.Revision < old.Revision || !served) {
		return nil
	}
	members, err := tenancy.OrgUsers(ctx, s.tenancy, next.ID)
	if err != nil {
		return err
	}
	for _, id := range members {
		if err := s.revokeUserSessions(id, ""); err != nil {
			return err
		}
	}
	s.logger.Info("org stopped; member sessions revoked", "action", "org_stop", "result", "committed", "count", strconv.Itoa(len(members)))
	return nil
}

// chargeOrgSend applies the org caps after the user's own charge id is written: it sums
// every member's charges, this one included, and drops this charge, if this call wrote
// it, when that passes
// sendPerHour or recipientsPerDay. Two concurrent sends can both be refused, never both
// admitted past the cap. Single mode and the staff org have no org cap.
func (s *Server) chargeOrgSend(ctx context.Context, id string, u users.User, now time.Time, inserted bool) error {
	if !s.multiTenant() || u.OrgID == s.staffOrg {
		return nil
	}
	o, err := tenancy.LookupOrg(ctx, s.tenancy, u.OrgID)
	q := o.Quotas
	if err == nil && q.SendPerHour == nil && q.RecipientsPerDay == nil {
		return nil
	}
	var members []string
	if err == nil {
		members, err = tenancy.OrgUsers(ctx, s.tenancy, u.OrgID)
	}
	messages, recipients := 0, 0
	if err == nil {
		// The sender's own user_orgs row may still be unwritten (PR 2's repair).
		members = append(members, u.ID)
		messages, recipients, err = s.globalStore.OrgSendUsage(ctx, dedupe(members), now.Unix())
	}
	if err == nil && (q.SendPerHour != nil && int64(messages) > *q.SendPerHour || q.RecipientsPerDay != nil && int64(recipients) > *q.RecipientsPerDay) {
		err = state.ErrSendQuota
	}
	// Only a charge this call wrote is released: a retry's earlier charge stays counted.
	if err != nil && inserted {
		if rerr := s.globalStore.ReleaseSend(ctx, id); rerr != nil {
			return errors.Join(err, rerr)
		}
	}
	return err
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// orgMailboxRoom refuses a new extra mailbox for u once its org holds its mailboxes
// quota of extra mailboxes, in any state. Lowering the quota keeps existing mailboxes.
// ponytail: two staff creations for one org at once can pass together; a per-org lock
// is the upgrade if staff tooling ever creates them in bulk.
func (s *Server) orgMailboxRoom(ctx context.Context, u users.User) error {
	if !s.multiTenant() || u.OrgID == s.staffOrg {
		return nil
	}
	o, err := tenancy.LookupOrg(ctx, s.tenancy, u.OrgID)
	if err != nil || o.Quotas.Mailboxes == nil {
		return err
	}
	mailboxes, err := s.ssoLifecycle.NativeMailboxes()
	if err != nil {
		return err
	}
	all, err := s.users.List()
	if err != nil {
		return err
	}
	org := map[string]string{}
	for _, a := range all {
		org[a.ID] = a.OrgID
	}
	n := int64(0)
	for _, m := range mailboxes {
		if m.Kind == "extra" && org[m.User] == u.OrgID {
			n++
		}
	}
	if n >= *o.Quotas.Mailboxes {
		return errOrgMailboxQuota
	}
	return nil
}

// exportOnlyLanding is where an export-only sign-in lands: the export page.
const exportOnlyLanding = "/settings/account?tab=export"

// exportOnlyRoutes are the only routes an export-only session reaches (K open question 4
// (a)): who it is, sign-out, the step-up that confirms an export, the mailbox list and the
// export itself. Any other answers as for a session that does not exist.
var exportOnlyRoutes = map[string]bool{
	"GET /api/auth/me": true, "GET /api/auth/csrf": true, "POST /api/auth/logout": true,
	"POST /api/auth/oidc/step-up": true, "GET /api/auth/oidc/step-up/{id}": true, "DELETE /api/auth/oidc/step-up/{id}": true,
	"GET /auth/sso/callback": true,
	"GET /api/mailboxes":     true, "GET /api/export/folders": true, "POST /api/export": true, "GET /api/export/{token}": true,
}

// orgExportable is nil while org's members may hold an export-only session: the cache
// knows it, fresh, as deletion_scheduled and enabled for KyPost. The staff org never is.
func (s *Server) orgExportable(ctx context.Context, org string) error {
	if !s.multiTenant() || org == s.staffOrg {
		return tenancy.ErrNotServed
	}
	o, err := tenancy.LookupOrg(ctx, s.tenancy, org)
	if err != nil {
		return err
	}
	if !o.Fresh(time.Now()) || o.Status != suitedir.OrgDeletionScheduled || !o.Enabled {
		return tenancy.ErrNotServed
	}
	return nil
}

// exportOnlySession reports whether r carries a live export-only session.
func (s *Server) exportOnlySession(r *http.Request) bool {
	ac, ok, err := s.currentUser(r)
	return err == nil && ok && ac.ExportOnly
}
