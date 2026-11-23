package api

import (
	"context"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// The org storage pool (tenancy design "Quotas"; KyIdentity "Org storage pool"). KyPost
// counts each org's stored mail, queued sends with their reserved Sent copies, mail held
// for import and contact photos; reports it with the org poll; and checks every write
// that adds bytes against the grant. Receiving is not checked (open question 16, the
// owner's), nor the held-mail import or a reserved Sent copy, already charged.

// minStorageWant is the least headroom asked for; more when the org grew faster.
const minStorageWant = 256 << 20

// orgUsage is the bytes KyPost stores for org now. held is the receiving spool's bytes
// per mailbox (nil without one).
// ponytail: one usage read per org mailbox and one photo directory read per member on
// every check; a per-org counter kept by the usage triggers is the upgrade when the
// capacity measurement (K open question 8) shows the cost.
func (s *Server) orgUsage(ctx context.Context, org string, held map[string]int64) (int64, error) {
	ids, err := tenancy.OrgUsers(ctx, s.tenancy, org)
	if err != nil {
		return 0, err
	}
	members := make(map[string]bool, len(ids))
	var total int64
	for _, id := range ids {
		members[id] = true
		photos, err := s.contactPhotoBytesUsed(id)
		if err != nil {
			return 0, err
		}
		total += photos
	}
	mailboxes, err := s.ssoLifecycle.NativeMailboxesWithoutAddresses()
	if err != nil {
		return 0, err
	}
	for _, m := range mailboxes {
		if !members[m.User] || m.Dir == "" {
			continue
		}
		u, err := mailbox.ReadUsage(filepath.Join(m.Dir, "mailbox"))
		if err != nil {
			return 0, err
		}
		total += u.Bytes + held[m.ID]
	}
	return total, nil
}

// heldBytes is the receiving spool's bytes per mailbox, nil when receiving was never set up.
func (s *Server) heldBytes(ctx context.Context) (map[string]int64, error) {
	h, err := s.openHolding()
	if err != nil || h == nil {
		return nil, err
	}
	defer h.Close()
	return h.HeldBytesByMailbox(ctx)
}

// orgPoolRoom is nil when org may store add more bytes now. The staff org is exempt; an
// org with no grant, an expired one, or one it would pass gets mailbox.ErrPoolFull.
func (s *Server) orgPoolRoom(ctx context.Context, org string, add int64) error {
	if !s.multiTenant() || add <= 0 || org != "" && org == s.staffOrg {
		return nil
	}
	if org == "" {
		return mailbox.ErrPoolFull
	}
	g, err := tenancy.StorageGrant(ctx, s.tenancy, org)
	if err != nil {
		return err
	}
	now := time.Now()
	if !g.Fits(0, add, now) {
		return mailbox.ErrPoolFull
	}
	held, err := s.heldBytes(ctx)
	if err != nil {
		return err
	}
	used, err := s.orgUsage(ctx, org, held)
	if err != nil {
		return err
	}
	if !g.Fits(used, add, now) {
		return mailbox.ErrPoolFull
	}
	return nil
}

// StoragePoolCheck is the mailbox.PoolCheck for this server's tenancy.db: the owner's org
// from subject_orgs, as receiving reads it.
func (s *Server) StoragePoolCheck(ctx context.Context, owner mailbox.Owner, add int64) error {
	org, err := tenancy.SubjectOrg(ctx, s.tenancy, owner.Issuer, owner.Subject)
	if err != nil {
		return err
	}
	return s.orgPoolRoom(ctx, org, add)
}

// reportStorage sends KyPost's usage for every cached org but the staff org and deleted
// ones, and records each grant. The expiry runs from the time the request was sent, so a
// slow answer only shortens it. A stale answer keeps the current grant; an unknown org's
// grant is zeroed. A failed batch is retried at the next poll.
func (s *Server) reportStorage(ctx context.Context) error {
	orgs, err := tenancy.ReportableOrgs(ctx, s.tenancy)
	if err != nil {
		return err
	}
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	if !ok {
		return errNoSuiteDirectory
	}
	held, err := s.heldBytes(ctx)
	if err != nil {
		return err
	}
	var batch []tenancy.StorageReport
	used := map[string]int64{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		sent := time.Now()
		results, err := tenancy.ReportStorage(ctx, dir, batch)
		if err != nil {
			return err
		}
		for i, r := range results {
			rep := batch[i]
			g := tenancy.Grant{Used: used[rep.OrgID]}
			switch r.Result {
			case "stale":
				continue
			case "ok":
				g.Ceiling = r.CeilingBytes
				g.Expires = sent.Add(min(time.Duration(r.TTLSeconds)*time.Second, tenancy.GrantTTL))
			}
			if err := tenancy.SetGrant(ctx, s.tenancy, rep.OrgID, rep.Seq, g); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	for _, org := range orgs {
		if org == s.staffOrg {
			continue
		}
		n, err := s.orgUsage(ctx, org, held)
		if err != nil {
			return err
		}
		seq, last, err := tenancy.NextStorageSeq(ctx, s.tenancy, org)
		if err != nil {
			return err
		}
		used[org] = n
		batch = append(batch, tenancy.StorageReport{OrgID: org, UsedBytes: n, Seq: seq, WantBytes: max(minStorageWant, 2*(n-last.Used))})
		if len(batch) == tenancy.MaxStorageReports {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
