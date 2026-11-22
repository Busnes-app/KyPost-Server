package api

import (
	"context"
	"errors"
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

// orgServed is orgServe for a credential check that answers a read failure as a refusal.
func (s *Server) orgServed(ctx context.Context, u users.User) bool {
	return s.orgServe(ctx, u.OrgID) == nil
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
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOrgs applies one /api/suite/orgs answer. An org that stops being served has its
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
	orgs, _, err := dir.Orgs(ctx, since)
	if err != nil {
		return err
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
	return tenancy.MarkPolled(ctx, s.tenancy, now)
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
// every member's charges, this one included, and drops this charge when that passes
// sendPerHour or recipientsPerDay. Two concurrent sends can both be refused, never both
// admitted past the cap. Single mode and the staff org have no org cap.
func (s *Server) chargeOrgSend(ctx context.Context, id string, u users.User, now time.Time) error {
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
	if err != nil {
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
