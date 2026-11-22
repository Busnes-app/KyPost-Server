package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// Multi-mode receiving import (RX R8): one obligation per bound mailbox, imported by
// workers on every daemon process under a per-mailbox lease, so no mailbox waits on
// another. Single mode keeps startReceivingImport's daemon importer.

const (
	importWorkers  = 4                // per daemon process
	importBatch    = 100              // obligations a lease holder attempts before letting go
	importBatchFor = 60 * time.Second // and the longest it holds a mailbox
	importSweep    = time.Minute      // the backstop Completion sweep
)

var (
	errBindingRetired = errors.New("the binding can never import")
	errImportWait     = errors.New("the mailbox's organisation is not in the entitlement cache yet")
	errLeaseLost      = errors.New("the mailbox's import lease was lost")
)

// startLeasedImport runs the import workers on every daemon process, outside the daemon
// role's election. It starts nothing unless KYPOST_TENANCY=multi.
func startLeasedImport(ctx context.Context, d runDeps) (<-chan struct{}, error) {
	done := make(chan struct{})
	mode, _, err := config.Tenancy()
	if err != nil {
		return nil, err
	}
	if !d.nativeReceiving || mode != config.TenancyMulti {
		close(done)
		return done, nil
	}
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	for range importWorkers {
		holder, err := fsutil.NewUUIDv4()
		if err != nil {
			_ = r.close()
			return nil, err
		}
		wg.Go(func() { r.importLoop(ctx, holder, d.logger) })
	}
	go func() {
		wg.Wait()
		_ = r.close()
		close(done)
	}()
	return done, nil
}

func (r *receivingRuntime) importLoop(ctx context.Context, holder string, logger *logging.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var skip map[string]bool
	for {
		skip = r.importPass(ctx, holder, skip, logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// importPass runs one batch for each mailbox with pending mail, except those whose batch
// hit its bound in the previous pass, so another worker can take them. It returns the
// mailboxes whose batch hit the bound this time.
func (r *receivingRuntime) importPass(ctx context.Context, holder string, skip map[string]bool, logger *logging.Logger) map[string]bool {
	mailboxes, err := r.holding.PendingMailboxes(ctx)
	if err != nil {
		logger.Error("receiving buffer unavailable; preserve storage and repair", "error", err.Error())
		return nil
	}
	next := map[string]bool{}
	for _, m := range mailboxes {
		if ctx.Err() != nil {
			break
		}
		if skip[m] {
			continue
		}
		more, err := r.importMailbox(ctx, holder, m, logger)
		if err != nil {
			logger.Error("receiving import deferred; holding bytes retained", "error", err.Error())
		}
		if more {
			next[m] = true
		}
	}
	return next
}

// importMailbox takes mailbox m's lease and attempts its outstanding obligations in
// arrival order: at most importBatch, for at most importBatchFor, renewing the lease
// before each. An obligation that fails is logged and retried next pass, as the single
// mode importer does. An org that is deleted or not cached stops the whole mailbox. more
// reports a batch cut short by its bound.
func (r *receivingRuntime) importMailbox(ctx context.Context, holder, m string, logger *logging.Logger) (more bool, err error) {
	start := time.Now()
	ok, err := tenancy.TakeImportLease(ctx, r.tenancy, m, holder, start)
	if err != nil || !ok {
		return false, err
	}
	defer func() {
		err = errors.Join(err, tenancy.ReleaseImportLease(context.WithoutCancel(ctx), r.tenancy, m, holder))
	}()
	var after int64
	attempted := 0
	for {
		page, err := r.holding.Obligations(ctx, m, after, 100)
		if err != nil || len(page) == 0 {
			return false, err
		}
		for _, o := range page {
			after = o.Sequence
			if attempted >= importBatch || time.Since(start) >= importBatchFor {
				return true, nil
			}
			if ok, err := tenancy.TakeImportLease(ctx, r.tenancy, m, holder, time.Now()); err != nil || !ok {
				return false, errors.Join(errLeaseLost, err)
			}
			tried, err := r.importObligation(ctx, holder, o, m)
			if tried {
				attempted++
			}
			switch {
			case errors.Is(err, errImportWait), errors.Is(err, tenancy.ErrImportDeleted):
				return false, nil
			case err != nil:
				logger.Error("receiving import deferred; holding bytes retained", "error", err.Error(), "correlation_id", o.ID)
			}
		}
	}
}

// importObligation imports delivery o into mailbox m unless m's obligation is done, then
// runs Completion. tried reports an attempt, as opposed to a done obligation skipped.
func (r *receivingRuntime) importObligation(ctx context.Context, holder string, o ingress.Obligation, m string) (tried bool, err error) {
	bindings, err := r.holding.Bindings(ctx, o.Gateway, o.ID)
	if err != nil {
		return false, err
	}
	if state, err := r.obligationDone(ctx, o.Gateway, o.ID, bindings, m); err != nil || state != ingress.NotDone {
		return false, err
	}
	d, err := r.holding.Get(ctx, o.Gateway, o.ID)
	if err != nil || d.State != "pending" {
		return false, err
	}
	// Bytes permanent storage refuses can import nowhere: quarantine the delivery, as
	// importDeliveryTo does.
	if mailbox.ImportValid(d.Raw, receivingLimits.MessageBytes) != nil {
		return true, r.holding.QuarantinePending(ctx, d.Gateway, d.ID)
	}
	if err := r.writeObligation(ctx, holder, d, m); err != nil {
		return true, err
	}
	return true, r.complete(ctx, d.Gateway, d.ID, d.Bindings)
}

// writeObligation is one obligation's steps 1 and 2: check the org, then record a durably
// retired binding, or write the message into m under its quota lock.
func (r *receivingRuntime) writeObligation(ctx context.Context, holder string, d ingress.Delivery, m string) error {
	var mine []ingress.Binding
	for _, b := range d.Bindings {
		if b.Mailbox == m {
			mine = append(mine, b)
		}
	}
	if len(mine) == 0 || !fsutil.SafePathComponent(m) {
		return sso.ErrNativeProvisioning
	}
	owner := mailbox.Owner{Issuer: mine[0].Issuer, Subject: mine[0].Subject, Mailbox: m}
	switch imp, err := tenancy.MailboxImport(ctx, r.tenancy, owner.Issuer, owner.Subject); {
	case err != nil:
		return err
	case imp == tenancy.ImportWait:
		return errImportWait
	case imp == tenancy.ImportNever:
		return tenancy.ErrImportDeleted
	}
	err := r.writeMailbox(ctx, holder, d, owner, mine)
	if errors.Is(err, errBindingRetired) || err != nil && !errors.Is(err, sso.ErrNativeRestoreHold) && !errors.Is(err, tenancy.ErrImportDeleted) && !errors.Is(err, errLeaseLost) && r.bindingsRetired(ctx, mine) {
		return tenancy.RecordRetired(ctx, r.tenancy, d.Gateway, d.ID, m, time.Now())
	}
	return err
}

// writeMailbox writes d's one copy into owner's mailbox under the authority fences, as
// importFrozen does for every mailbox. Under the quota lock it re-reads the org: the purge
// marks the org deleted before it takes the same lock to remove the storage, so a write
// either lands before the removal or sees deleted and writes nothing. It also renews
// holder's lease there, so a worker whose lease ran out while it waited for the locks
// writes nothing and cannot overtake the mailbox's new holder.
func (r *receivingRuntime) writeMailbox(ctx context.Context, holder string, d ingress.Delivery, owner mailbox.Owner, mine []ingress.Binding) error {
	a, found, err := r.life.NativeMailboxAssignment(owner.Mailbox)
	if err != nil || !found || a.Owner != owner || int64(len(d.Raw)) > a.Limits.MessageBytes {
		return sso.ErrNativeProvisioning
	}
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(r.stateDir), "mailbox"), owner, a.Limits, a.Source)
	if err != nil {
		return err
	}
	defer store.Close()
	client, err := mailbox.NewClient(store, a.Address)
	if err != nil {
		return err
	}
	source := client.MailSourceIdentity()
	addresses := make([]string, 0, len(mine))
	for _, b := range mine {
		addresses = append(addresses, b.Address)
	}
	return r.withAuthority(ctx, true, []string{owner.Mailbox}, addresses, nil, func(current map[string]sso.NativeAssignment) error {
		admitted := current[owner.Mailbox]
		if admitted.Owner != owner {
			return errBindingRetired
		}
		for _, b := range mine {
			x, err := r.life.NativeAddress(b.Address)
			if err != nil {
				return err
			}
			if x.Mailbox != b.Mailbox || x.State != "active" || x.Generation != b.Generation {
				return errBindingRetired
			}
		}
		if admitted.Source != source {
			return sso.ErrNativeProvisioning
		}
		return r.quotaFence(ctx, map[string]sso.NativeAssignment{owner.Mailbox: admitted}, func() error {
			imp, err := tenancy.MailboxImport(ctx, r.tenancy, owner.Issuer, owner.Subject)
			if err != nil {
				return err
			}
			if imp == tenancy.ImportNever {
				return tenancy.ErrImportDeleted
			}
			if ok, err := tenancy.TakeImportLease(ctx, r.tenancy, owner.Mailbox, holder, time.Now()); err != nil || !ok {
				return errors.Join(errLeaseLost, err)
			}
			return r.holding.ImportMailbox(ctx, d, "INBOX", store)
		})
	})
}

// obligationDone reads mailbox m's obligation for delivery (gateway, id): retired when
// recorded so, imported when m holds the receipt.
func (r *receivingRuntime) obligationDone(ctx context.Context, gateway, id string, bindings []ingress.Binding, m string) (ingress.Done, error) {
	retired, err := tenancy.Retired(ctx, r.tenancy, gateway, id, m)
	if err != nil || retired {
		return ingress.Retired, err
	}
	i := slices.IndexFunc(bindings, func(b ingress.Binding) bool { return b.Mailbox == m })
	if i < 0 || !fsutil.SafePathComponent(m) {
		return ingress.NotDone, nil
	}
	owner := mailbox.Owner{Issuer: bindings[i].Issuer, Subject: bindings[i].Subject, Mailbox: m}
	a, found, err := r.life.NativeMailboxAssignment(m)
	if err != nil || !found || a.Owner != owner {
		return ingress.NotDone, err
	}
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(r.stateDir), "mailbox"), owner, a.Limits, a.Source)
	if err != nil {
		return ingress.NotDone, err
	}
	defer store.Close()
	has, err := store.HasReceipt(ctx, gateway, id)
	if err != nil || !has {
		return ingress.NotDone, err
	}
	return ingress.Imported, nil
}

// complete is Completion: it reads every bound mailbox's obligation, after the caller's
// own write committed, and ends the delivery when all are done. It stops at the first
// open one.
func (r *receivingRuntime) complete(ctx context.Context, gateway, id string, bindings []ingress.Binding) error {
	done := map[string]ingress.Done{}
	for _, b := range bindings {
		if _, seen := done[b.Mailbox]; seen {
			continue
		}
		state, err := r.obligationDone(ctx, gateway, id, bindings, b.Mailbox)
		if err != nil || state == ingress.NotDone {
			return err
		}
		done[b.Mailbox] = state
	}
	_, err := r.holding.Complete(ctx, gateway, id, done)
	return err
}

// sweepCompletions is the backstop: Completion for every pending delivery, for one whose
// last obligation committed before its worker crashed.
func (r *receivingRuntime) sweepCompletions(ctx context.Context, logger *logging.Logger) {
	gateways, err := r.holding.PendingGateways(ctx)
	if err != nil {
		logger.Error("receiving buffer unavailable; preserve storage and repair", "error", err.Error())
		return
	}
	for _, gateway := range gateways {
		var after int64
		for ctx.Err() == nil {
			rows, err := r.holding.List(ctx, gateway, after, 100)
			if err != nil || len(rows) == 0 {
				break
			}
			for _, row := range rows {
				after = row.Sequence
				if row.State != "pending" {
					continue
				}
				bindings, err := r.holding.Bindings(ctx, gateway, row.ID)
				if err == nil {
					err = r.complete(ctx, gateway, row.ID, bindings)
				}
				if err != nil {
					logger.Error("receiving completion deferred; holding bytes retained", "error", err.Error(), "correlation_id", row.ID)
				}
			}
		}
	}
}
