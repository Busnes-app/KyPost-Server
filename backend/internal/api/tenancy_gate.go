package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The org gate (docs/superpowers/specs/2026-11-20-tenancy-design.md, K5, "The cross-user
// hops" and "PR 3 as built"). Every cross-user hop calls one of these; a single-tenant
// install admits everything here and compares no org. TestOrgAdmitCallSites fails the
// build when an accessor is reached with an ID that did not come from the caller's own
// AuthContext and the function calls none of them.

// orgAdmit admits a hop from the caller to target: in multi mode both must carry the
// same org. A refusal must answer exactly as a missing target does.
func (s *Server) orgAdmit(ac AuthContext, target users.User) bool {
	return !s.multiTenant() || ac.OrgID != "" && target.OrgID == ac.OrgID
}

// peerOrgAdmit admits a signed inbound peer body for u, its (sub, issuer)'s account: in
// multi mode the body's top-level org_id must equal the account's. A peer that sends none
// is refused; a refusal answers like an unknown sub.
func (s *Server) peerOrgAdmit(body []byte, u users.User) bool {
	if !s.multiTenant() {
		return true
	}
	var b struct {
		OrgID string `json:"org_id"`
	}
	return json.Unmarshal(body, &b) == nil && config.ValidOrgID(b.OrgID) && s.orgAdmit(AuthContext{OrgID: b.OrgID}, u)
}

// sameOrgPeople keeps the directory answers whose subject this install has bound to the
// caller's org. KyIdentity already scopes them (T "Suite directory"), but suitedir.Person
// carries no org_id, so in multi mode KyPost checks its own subject_orgs; an unknown
// subject is dropped.
func (s *Server) sameOrgPeople(ctx context.Context, u users.User, issuer string, people []suitedir.Person) []suitedir.Person {
	if !s.multiTenant() {
		return people
	}
	return slices.DeleteFunc(people, func(p suitedir.Person) bool {
		org, err := tenancy.SubjectOrg(ctx, s.tenancy, issuer, p.Sub)
		return err != nil || org == "" || !s.orgAdmit(AuthContext{OrgID: u.OrgID}, users.User{OrgID: org})
	})
}

// addressOneOrg admits moving address to mailboxID (a reassign, or a quarantine release to
// the current owner) only when every mailbox that has held it, and the target, belong to
// accounts of one org. A ledger read failure refuses.
func (s *Server) addressOneOrg(address, mailboxID string) (bool, error) {
	if !s.multiTenant() {
		return true, nil
	}
	held, err := s.ssoLifecycle.NativeAddressMailboxes(address)
	if err != nil {
		return false, err
	}
	mailboxes, err := s.ssoLifecycle.NativeMailboxes()
	if err != nil {
		return false, err
	}
	owner := map[string]string{}
	for _, m := range mailboxes {
		owner[m.ID] = m.User
	}
	target, err := s.users.Get(owner[mailboxID])
	if err != nil {
		return false, nil
	}
	for _, id := range held {
		u, err := s.users.Get(owner[id])
		if err != nil || !s.orgAdmit(AuthContext{OrgID: target.OrgID}, u) {
			return false, nil
		}
	}
	return len(held) > 0, nil
}

// withOrg adds u's org_id to an outbound peer body, for the peers whose own spec reads it
// (KyCalendar and KyDrive tenancy designs). Only a multi-tenant account has an OrgID, so a
// single-tenant body is unchanged.
func withOrg(body map[string]any, u users.User) map[string]any {
	if u.OrgID != "" {
		body["org_id"] = u.OrgID
	}
	return body
}

// strictPeerOrg is the org_id field of a peer body decoded with DisallowUnknownFields. A
// single-tenant install refuses the field as before; multi mode reads it in peerOrgAdmit.
type strictPeerOrg struct {
	OrgID json.RawMessage `json:"org_id"`
}

func (s *Server) strictPeerOrgOK(f strictPeerOrg) bool {
	return s.multiTenant() || f.OrgID == nil
}

// deviceOrg is a paired device's account org, read from the user record. Multi mode
// refuses an account without one like a revoked credential; single mode reads nothing.
func (s *Server) deviceOrg(userID string) (string, bool) {
	if !s.multiTenant() {
		return "", true
	}
	u, err := s.users.Get(userID)
	return u.OrgID, err == nil && u.OrgID != ""
}

var errQuarantineOtherOrg = fmt.Errorf("%w: the address has been held in another org", sso.ErrQuarantineRelease)

// quarantineOneOrg admits releasing an unresolved delivery to mailboxID, its address's
// current owner, only when addressOneOrg holds (K "Quarantine release to the current
// owner"). A resolved or multi-recipient delivery is left to ReleaseQuarantined, which
// refuses both for a reviewed mailbox.
func (s *Server) quarantineOneOrg(ctx context.Context, holding *ingress.Store, gateway, id, mailboxID string) error {
	if !s.multiTenant() {
		return nil
	}
	d, err := holding.Get(ctx, gateway, id)
	if err != nil || len(d.Bindings) != 1 {
		return err
	}
	ok, err := s.addressOneOrg(d.Bindings[0].Address, mailboxID)
	if err == nil && !ok {
		err = errQuarantineOtherOrg
	}
	return err
}
