package api

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
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

// readOrgMailboxUsage is the report's storage-read seam; writes never call it.
var readOrgMailboxUsage = mailbox.ReadUsage

// orgUsage measures only this org's member mailboxes and photos, during a report.
// Ownership is queried per member, never through the install-wide ledger.
func (s *Server) orgUsage(ctx context.Context, org string, held func(mailbox string) (int64, error)) (int64, error) {
	ids, err := tenancy.OrgUsers(ctx, s.tenancy, org)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, id := range ids {
		photos, err := s.contactPhotoBytesUsed(id)
		if err != nil {
			return 0, err
		}
		total += photos
		boxes, err := s.ssoLifecycle.NativeMailboxesOf(id)
		if err != nil {
			return 0, err
		}
		for _, m := range boxes {
			if m.Dir == "" {
				continue
			}
			u, err := readOrgMailboxUsage(filepath.Join(m.Dir, "mailbox"))
			if err != nil {
				return 0, err
			}
			h, err := held(m.ID)
			if err != nil {
				return 0, err
			}
			total += u.Bytes + h
		}
	}
	return total, nil
}

// withHolding runs f with the receiving spool, or nil when receiving was never set up.
func (s *Server) withHolding(f func(h *ingress.Store) error) error {
	h, err := s.openHolding()
	if err != nil {
		return err
	}
	if h != nil {
		defer h.Close()
	}
	return f(h)
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
	ok, err := tenancy.ReserveStorage(ctx, s.tenancy, org, add, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return mailbox.ErrPoolFull
	}
	return nil
}

// StoragePoolCheck is the mailbox.PoolCheck for this server's tenancy.db: the owner's org
// from subject_orgs, as receiving reads it.
func (s *Server) StoragePoolCheck(ctx context.Context, owner mailbox.Owner, add int64) error {
	if !s.multiTenant() {
		return nil
	}
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
	// Open the spool once per report, but read only each org's own mailboxes.
	return s.withHolding(func(h *ingress.Store) error {
		heldOf := func(id string) (int64, error) {
			if h == nil {
				return 0, nil
			}
			return h.HeldBytes(ctx, id)
		}
		var batch []tenancy.StorageReport
		used := map[string]tenancy.Grant{}
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
				g := used[rep.OrgID]
				switch r.Result {
				case "stale":
					continue
				case "ok":
					g.Ceiling = r.CeilingBytes
					g.Expires = sent.Add(time.Duration(min(r.TTLSeconds, int64(tenancy.GrantTTL/time.Second))) * time.Second)
 g.PoolBytes, g.OrgUsed = r.PoolBytes, r.OrgUsedBytes
 if r.OverSince != nil { g.OverSince = time.Unix(*r.OverSince, 0) }
				}
				if err := tenancy.SetGrant(ctx, s.tenancy, rep.OrgID, rep.Seq, g); err != nil {
					return err
				}
			}
			batch = batch[:0]
			return nil
		}
		// One org whose usage or sequence cannot be read is skipped, so it alone fails closed
		// when its grant expires; the others are still reported.
		skipped := 0
		for _, org := range orgs {
			if org == s.staffOrg {
				continue
			}
			snapshot, err := tenancy.StorageGrant(ctx, s.tenancy, org)
			if err != nil {
				skipped++
				continue
			}
			n, err := s.orgUsage(ctx, org, heldOf)
			if err != nil {
				skipped++
				continue
			}
			seq, last, err := tenancy.NextStorageSeq(ctx, s.tenancy, org)
			if err != nil {
				skipped++
				continue
			}
			used[org] = tenancy.Grant{Used: n, Admitted: snapshot.Admitted}
			batch = append(batch, tenancy.StorageReport{OrgID: org, UsedBytes: n, Seq: seq, WantBytes: max(minStorageWant, 2*(n-last.Reported))})
			if len(batch) == tenancy.MaxStorageReports {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if skipped > 0 {
			// The errors carry paths and IDs; the line carries only the count.
			s.logger.Error("storage report: skipped orgs whose usage or sequence could not be read; their grants expire", "skipped", strconv.Itoa(skipped))
		}
		return flush()
	})
}
