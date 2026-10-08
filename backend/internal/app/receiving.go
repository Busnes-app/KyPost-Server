package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const receivingGateway = "maddy-local"

var receivingLimits = ingress.ReceivingLimits

// errMailboxMessageLimit is one message over one frozen mailbox's limit, not
// a full receiving store.
var errMailboxMessageLimit = fmt.Errorf("%w: the message exceeds a recipient mailbox's per-message limit", ingress.ErrCapacity)

// errMailboxFull is a recipient mailbox at its quota: a temporary refusal
// (exit 9, 452 4.2.2), so the sender retries once its owner frees space. It
// is not ErrCapacity: hosted pickup leaves only this message waiting in R2.
var errMailboxFull = errors.New("recipient mailbox is full")

// mailboxRoom refuses a message of size bytes that a's mailbox cannot hold,
// counting accepted mail still waiting to import into it. Acceptance runs
// under the directory fence, so concurrent deliveries see each other.
func (r *receivingRuntime) mailboxRoom(ctx context.Context, a sso.NativeAssignment, size int64) error {
	u, err := mailbox.ReadUsage(filepath.Join(a.Dir(r.stateDir), "mailbox"))
	if err != nil {
		return err
	}
	held, err := r.holding.HeldBytes(ctx, a.Owner.Mailbox)
	if err != nil {
		return err
	}
	u.Bytes += held
	if !u.Fits(a.Limits, size) {
		return &receivingCommandError{err: errMailboxFull, code: 9}
	}
	return nil
}

type receivingRuntime struct {
	gateway   string
	configDir string
	stateDir  string
	accounts  *users.Store
	life      *sso.LifecycleStore
	domains   *sso.NativeDomainStore
	holding   *ingress.Store
}

func (r *receivingRuntime) gatewayID() string {
	if r.gateway == "" {
		return receivingGateway
	}
	return r.gateway
}

type receivingCommandError struct {
	err  error
	code int
}

func (e *receivingCommandError) Error() string { return e.err.Error() }
func (e *receivingCommandError) Unwrap() error { return e.err }
func (e *receivingCommandError) ExitCode() int { return e.code }

func runReceivingCommand(args []string, input io.Reader) error {
	if len(args) == 0 || (args[0] != "init" && args[0] != "bind" && args[0] != "accept") ||
		(args[0] == "init" && len(args) != 1) || (args[0] == "bind" && len(args) != 4) || (args[0] == "accept" && len(args) != 3 && len(args) != 5) {
		return errors.New("usage: receiving init | receiving bind <receiver-id> <sender> <recipient> | receiving accept <receiver-id> <sender> [<peer-ip> <helo>]")
	}
	if len(args) > 1 {
		if args[1] == "" || len(args[1]) > 256 || strings.ContainsAny(args[1], "\x00\r\n") {
			return errors.New("invalid receiver transaction identifier")
		}
		if args[2] != "" {
			a, err := mail.ParseAddress(args[2])
			if err != nil || a.Name != "" || a.Address != args[2] {
				code := 1
				if args[0] == "bind" {
					code = 7
				}
				return &receivingCommandError{err: errInvalidSender, code: code}
			}
		}
		if args[0] == "bind" {
			a, err := mail.ParseAddress(args[3])
			if err != nil || a.Name != "" || a.Address != args[3] {
				return &receivingCommandError{err: ingress.ErrRoute, code: 3}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := openReceivingRuntime(ctx, args[0] == "init")
	replayed := false
	if err == nil {
		defer r.holding.Close()
		// Archived is terminal, so checking first cannot mislabel new mail.
		if args[0] != "init" {
			d, errGet := r.holding.Get(ctx, r.gatewayID(), args[1])
			replayed = errGet == nil && d.State == "archived"
		}
		switch args[0] {
		case "bind":
			err = r.bind(ctx, args[1], args[2], args[3])
		case "accept":
			err = r.accept(ctx, args[1], args[2], input, args[3:]...)
		}
	}
	result := "committed"
	if err != nil {
		result = "refused"
	} else if replayed {
		result = "replayed"
	}
	correlation := "initialization"
	if len(args) > 1 {
		correlation = args[1]
	}
	slog.Info("receiving operation", "actor", receivingGateway, "task_id", "native-receiving", "action", args[0], "target", "holding-store", "result", result, "correlation_id", correlation)
	if err != nil {
		return &receivingCommandError{err: err, code: receivingExit(err)}
	}
	return nil
}

// receivingExit is the helper's exit code for a refusal; receiving_config.go
// maps each to an SMTP reply.
func receivingExit(err error) int {
	var commandError *receivingCommandError
	switch {
	case errors.Is(err, ingress.ErrRoute):
		return 3
	case errors.Is(err, fsutil.ErrDriveReserve):
		return 10
	case errors.As(err, &commandError):
		return commandError.code
	}
	return 1
}

// startReceivingImport uses the daemon's cancellation/drain ownership. Missing
// state refuses startup; accepted mail is never replaced with an empty spool.
func startReceivingImport(ctx context.Context, d runDeps) (<-chan struct{}, error) {
	done := make(chan struct{})
	if !d.nativeReceiving {
		close(done)
		return done, nil
	}
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(done)
		defer r.holding.Close()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			for _, gateway := range []string{receivingGateway, cloudflareGateway} {
				scoped := *r
				scoped.gateway = gateway
				var after int64
				for ctx.Err() == nil {
					rows, err := r.holding.List(ctx, gateway, after, 100)
					if err != nil {
						d.logger.Error("receiving buffer unavailable; preserve storage and repair", "error", err.Error())
						break
					}
					if len(rows) == 0 {
						break
					}
					for _, row := range rows {
						after = row.Sequence
						if row.State != "pending" || ctx.Err() != nil {
							continue
						}
						if err := scoped.importDelivery(ctx, row.ID); err != nil {
							d.logger.Error("receiving import deferred; holding bytes retained", "error", err.Error(), "correlation_id", row.ID)
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done, nil
}

func openReceivingRuntime(ctx context.Context, initialize bool) (*receivingRuntime, error) {
	native, err := config.NativeMailEnabled()
	if err != nil {
		return nil, err
	}
	receiving, err := config.NativeReceivingEnabled()
	if err != nil {
		return nil, err
	}
	if !native || !receiving {
		return nil, errors.New("receiving requires explicit KYPOST_NATIVE_MAIL=true and KYPOST_NATIVE_RECEIVING=true")
	}
	r := &receivingRuntime{configDir: config.ConfigDir(), stateDir: config.StateDir()}
	for _, path := range []string{r.configDir, r.stateDir} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("receiving requires existing owner-only configuration and state directories")
		}
	}
	if err := sso.RequireNativeRestoreReleased(r.stateDir); err != nil {
		return nil, err
	}
	r.accounts, err = users.OpenExisting(ctx, r.configDir)
	if err != nil {
		return nil, err
	}
	r.life = sso.NewLifecycleStore(r.configDir)
	r.domains = sso.NewNativeDomainStore(r.configDir)
	settings := sso.NewStore(r.configDir).Load()
	domains, err := r.domains.ReadSet()
	established := []string{}
	for name, d := range domains.Domains {
		if d.Established {
			established = append(established, name)
		}
	}
	if err != nil || !settings.Enabled || domains.Issuer != settings.IssuerURL || len(established) == 0 {
		return nil, sso.ErrNativeDomain
	}
	path := filepath.Join(r.stateDir, "receiving")
	if initialize {
		// One currently proven domain suffices; a lapsed one never blocks others.
		proven := false
		for _, name := range established {
			if _, err := r.domains.VerifyDomain(ctx, name); err == nil {
				proven = true
				break
			}
		}
		if !proven {
			return nil, sso.ErrNativeDomain
		}
		// Explicit initialization never repairs a lost acknowledged holding root.
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("receiving directory already exists; open or reconcile it instead of initializing")
		}
		r.holding, err = ingress.Open(path, receivingLimits)
	} else {
		r.holding, err = ingress.OpenExisting(path, receivingLimits)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// verifyDomains freshly proves each distinct recipient domain. A delivery to
// several domains is all-or-nothing, so one lapsed domain delays only it.
// A domain outside the set (or retired) is no route at all.
func (r *receivingRuntime) verifyDomains(ctx context.Context, addresses []string) ([]sso.NativeDomain, error) {
	domains, err := r.domains.ReadSet()
	if err != nil {
		return nil, err
	}
	proofs := []sso.NativeDomain{}
	seen := map[string]bool{}
	for _, address := range addresses {
		domain := sso.AddressDomain(address)
		if seen[domain] {
			continue
		}
		seen[domain] = true
		if _, configured := domains.Domains[domain]; !configured {
			return nil, ingress.ErrRoute
		}
		proof, err := r.domains.VerifyDomain(ctx, domain)
		if err != nil {
			return nil, err
		}
		proofs = append(proofs, proof)
	}
	return proofs, nil
}

// withAuthority fences only the recipients' domains: each must still be an
// established member of the set, and every supplied proof still current.
// Import supplies none, since accepted bytes are owed without fresh DNS.
func (r *receivingRuntime) withAuthority(ctx context.Context, ids, addresses []string, proofs []sso.NativeDomain, action func(map[string]sso.NativeAssignment) error) error {
	// Match allocator lock order. DNS and stdin work happen before this fence.
	release, err := fsutil.LockFileContext(ctx, filepath.Join(r.configDir, sso.NativeDomainsFile))
	if err != nil {
		return err
	}
	defer release()
	domains, err := r.domains.ReadSet()
	settings := sso.NewStore(r.configDir).Load()
	if err != nil || !settings.Enabled || domains.Issuer != settings.IssuerURL {
		return sso.ErrNativeDomain
	}
	for _, address := range addresses {
		if !domains.Domains[sso.AddressDomain(address)].Established {
			return sso.ErrNativeDomain
		}
	}
	for _, proof := range proofs {
		if !domains.CurrentProof(proof) {
			return sso.ErrNativeDomain
		}
	}
	return r.life.WithNativeMailAccess(ctx, r.stateDir, settings.IssuerURL, r.accounts, ids, action)
}

// errInvalidSender is a sender shape neither profile accepts.
var errInvalidSender = errors.New("invalid envelope sender")

// bind is the Maddy RCPT check, before any DNS proof or storage: a sender
// the Worker would refuse exits 7 (550), so every accepted sender can be
// blocked in both profiles; an unreadable block list exits 8 (451, mail
// retried); a blocked sender exits 6 (550). Hosted pickup binds mail the
// gateway already accepted and never checks either.
func (r *receivingRuntime) bind(ctx context.Context, id, sender, recipient string) error {
	if !cfreceiving.ValidSender(sender) {
		return &receivingCommandError{err: errInvalidSender, code: 7}
	}
	blocked, err := ingress.NewBlocks(filepath.Join(r.stateDir, "receiving")).Blocked(sender, time.Now())
	if err != nil {
		return &receivingCommandError{err: fmt.Errorf("sender blocks unreadable: %w", err), code: 8}
	}
	if blocked {
		return &receivingCommandError{err: ingress.ErrSenderBlock, code: 6}
	}
	return r.bindExpected(ctx, id, sender, recipient, nil)
}

// bindExpected is bind for a hosted gateway that froze its routing earlier:
// expected must accept the current admitted assignment and address.
func (r *receivingRuntime) bindExpected(ctx context.Context, id, sender, recipient string, expected func(sso.NativeAssignment, sso.NativeAddress) bool) error {
	parsed, err := mail.ParseAddress(recipient)
	if err != nil || parsed.Name != "" || parsed.Address != recipient {
		return ingress.ErrRoute
	}
	recipient = strings.ToLower(recipient)
	proofs, err := r.verifyDomains(ctx, []string{recipient})
	if err != nil {
		return err
	}
	a, found, err := r.life.NativeAssignmentForAddress(proofs[0].Issuer, recipient)
	if err != nil {
		return err
	}
	if !found {
		return ingress.ErrRoute
	}
	return r.withAuthority(ctx, []string{a.Owner.Mailbox}, []string{recipient}, proofs, func(current map[string]sso.NativeAssignment) error {
		admitted := current[a.Owner.Mailbox]
		x, err := r.activeAddress(recipient)
		if err != nil {
			return err
		}
		if admitted.Owner != a.Owner || x.Mailbox != a.Owner.Mailbox {
			return ingress.ErrRoute
		}
		if expected != nil && !expected(admitted, x) {
			return ingress.ErrRoute
		}
		// RCPT carries no size, and a DATA refusal refuses every recipient:
		// a mailbox without room for one more largest message refuses here,
		// alone, so a retry delivers to the others.
		if err := r.mailboxRoom(ctx, admitted, admitted.Limits.MessageBytes); err != nil {
			return err
		}
		if err := r.refreshRoute(ctx, admitted, x, false); err != nil {
			return err
		}
		return r.holding.Bind(ctx, r.gatewayID(), id, sender, recipient)
	})
}

// activeAddress reads the current ledger record under the caller's directory
// fence; only an active address routes.
func (r *receivingRuntime) activeAddress(address string) (sso.NativeAddress, error) {
	addresses, err := r.life.NativeAddresses()
	if err != nil {
		return sso.NativeAddress{}, err
	}
	x, ok := addresses[address]
	if !ok || x.State != "active" {
		return x, ingress.ErrRoute
	}
	return x, nil
}

// Routes carry the address generation, which changes only on reassign,
// disable, re-enable and release, so ordinary directory edits never fence
// staged or accepted bindings.
func (r *receivingRuntime) refreshRoute(ctx context.Context, a sso.NativeAssignment, x sso.NativeAddress, recovery bool) error {
	route := ingress.Route{Address: x.Address, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox, Generation: x.Generation, Active: true, ValidUntil: time.Now().Add(time.Minute)}
	if recovery {
		return r.holding.RefreshRoute(ctx, route)
	}
	return r.holding.SetRoute(ctx, route)
}

func (r *receivingRuntime) frozenAuthority(ctx context.Context, d ingress.Delivery, proofs []sso.NativeDomain, action func(map[string]sso.NativeAssignment) error) error {
	ids := make([]string, 0, len(d.Bindings))
	for _, b := range d.Bindings {
		ids = append(ids, b.Mailbox)
	}
	return r.withAuthority(ctx, ids, bindingAddresses(d), proofs, func(current map[string]sso.NativeAssignment) error {
		quarantine := func() error {
			if d.State == "pending" {
				if err := r.holding.QuarantinePending(ctx, r.gatewayID(), d.ID); err != nil {
					return err
				}
			}
			return ingress.ErrRoute
		}
		addresses, err := r.life.NativeAddresses()
		if err != nil {
			return err
		}
		for _, b := range d.Bindings {
			a, x := current[b.Mailbox], addresses[b.Address]
			if a.Owner != (mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}) || x.Mailbox != b.Mailbox || x.State != "active" || x.Generation != b.Generation {
				return quarantine()
			}
			if err := r.refreshRoute(ctx, a, x, proofs == nil); err != nil {
				return err
			}
		}
		return action(current)
	})
}

func (r *receivingRuntime) accept(ctx context.Context, id, sender string, input io.Reader, peer ...string) error {
	enabled, err := receivingRspamdEnabled()
	if err != nil {
		return err
	}
	if r.gatewayID() == cloudflareGateway && !enabled {
		return errors.New("cloudflare pickup requires KYPOST_RECEIVING_RSPAMD=true")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if file, ok := input.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		// Regular files have bounded reads but retain the filesystem-stall
		// ceiling. SMTP helper stdin is a pipe and must support a real deadline.
		if !info.Mode().IsRegular() {
			deadline, _ := ctx.Deadline()
			// Inherited blocking stdin is not registered with Go's poller.
			// Reopen the Linux pipe through its descriptor so os.Open supplies
			// the pollable handle needed for a kernel-backed read deadline.
			if info.Mode()&os.ModeNamedPipe == 0 {
				return errors.New("receiving stdin must be a pipe or regular file")
			}
			pipe, err := os.Open("/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10))
			if err != nil {
				return err
			}
			defer pipe.Close()
			if err := pipe.SetReadDeadline(deadline); err != nil {
				return errors.New("receiving stdin cannot enforce its deadline; configure a supported pipe")
			}
			input = pipe
		}
	}
	raw, err := io.ReadAll(io.LimitReader(input, receivingLimits.MessageBytes+1))
	if err != nil {
		return err
	}
	if len(raw) == 0 || int64(len(raw)) > receivingLimits.MessageBytes {
		return ingress.ErrCapacity
	}
	d, err := r.holding.Get(ctx, r.gatewayID(), id)
	if err != nil {
		return err
	}
	if d.State == "archived" {
		// Completed delivery: only an exact replay succeeds; nothing is written.
		return r.holding.Accept(ctx, r.gatewayID(), id, sender, bytes.NewReader(raw))
	}
	proofs, err := r.verifyDomains(ctx, bindingAddresses(d))
	if err != nil {
		return err
	}
	// Only a direct scan fills auth (receivingAuthentication); hosted
	// gateways and an absent scanner leave it empty, so they feed nothing.
	var auth ingress.Authentication
	if enabled && d.State == "staged" {
		if d.Sender != sender || r.gatewayID() != cloudflareGateway && len(peer) != 2 || r.gatewayID() == cloudflareGateway && len(peer) != 0 {
			return &receivingCommandError{err: errors.New("rspamd requires the bound sender and receiver-supplied IP/HELO"), code: 5}
		}
		ip, helo := "", ""
		if len(peer) == 2 {
			ip, helo = peer[0], peer[1]
		}
		var err error
		if auth, err = scanReceivingSpam(ctx, raw, d, ip, helo, receivingRspamdURL); errors.Is(err, errSpamReject) {
			r.senderEvidence(ctx, d.ID, auth, true)
		}
		if err != nil {
			return err
		}
	}
	if err := r.commitAccept(ctx, d, sender, raw, proofs); err != nil {
		return err
	}
	r.senderEvidence(ctx, d.ID, auth, false)
	return nil
}

// commitAccept stores scanned bytes for a staged delivery under its frozen
// owners' current authority and their per-message limits.
func (r *receivingRuntime) commitAccept(ctx context.Context, d ingress.Delivery, sender string, raw []byte, proofs []sso.NativeDomain) error {
	return r.frozenAuthority(ctx, d, proofs, func(current map[string]sso.NativeAssignment) error {
		for _, a := range current {
			if int64(len(raw)) > a.Limits.MessageBytes {
				return errMailboxMessageLimit
			}
			if err := r.mailboxRoom(ctx, a, int64(len(raw))); err != nil {
				return err
			}
		}
		return r.holding.Accept(ctx, r.gatewayID(), d.ID, sender, bytes.NewReader(raw))
	})
}

func bindingAddresses(d ingress.Delivery) []string {
	addresses := make([]string, 0, len(d.Bindings))
	for _, b := range d.Bindings {
		addresses = append(addresses, b.Address)
	}
	return addresses
}

func (r *receivingRuntime) importDelivery(ctx context.Context, id string) error {
	return r.importDeliveryTo(ctx, id, "INBOX")
}

func (r *receivingRuntime) importDeliveryTo(ctx context.Context, id, folder string) error {
	d, err := r.holding.Get(ctx, r.gatewayID(), id)
	if err != nil {
		return err
	}
	// A lost pickup response may repeat a completed local obligation.
	if d.State == "archived" {
		return nil
	}
	err = r.importFrozen(ctx, d, folder)
	if err != nil && d.State == "pending" && !errors.Is(err, ingress.ErrRoute) && !errors.Is(err, sso.ErrNativeRestoreHold) {
		if stale := r.quarantineRetired(ctx, d); stale != nil {
			return stale
		}
	}
	return err
}

// quarantineRetired runs after import was refused, usually by admission
// before the generation check could run. It quarantines the delivery when the
// ledger durably records a frozen address as moved, inactive or at a newer
// generation: an administrator disabled the mailbox or released the address,
// or the directory offboarded or promoted the owner. Such a binding can never
// import again, and would otherwise hold receiving capacity forever. Directory
// lag, storage or lock failures and missing sign-on settings change no
// address, so that mail stays pending and retries. Returns nil when nothing
// is proven stale.
func (r *receivingRuntime) quarantineRetired(ctx context.Context, d ingress.Delivery) error {
	release, err := r.life.LockDirectoryContext(ctx)
	if err != nil {
		return nil
	}
	defer release()
	addresses, err := r.life.NativeAddresses()
	if err != nil {
		return nil
	}
	if retiredBinding(addresses, d.Bindings) {
		if err := r.holding.QuarantinePending(ctx, r.gatewayID(), d.ID); err != nil {
			return err
		}
		return ingress.ErrRoute
	}
	return nil
}

func (r *receivingRuntime) importFrozen(ctx context.Context, d ingress.Delivery, folder string) error {
	id := d.ID
	stores := map[mailbox.Owner]*mailbox.Store{}
	sources := map[mailbox.Owner]string{}
	defer func() {
		for _, store := range stores {
			_ = store.Close()
		}
	}()
	for _, b := range d.Bindings {
		owner := mailbox.Owner{Issuer: b.Issuer, Subject: b.Subject, Mailbox: b.Mailbox}
		if !fsutil.SafePathComponent(owner.Mailbox) {
			return sso.ErrNativeProvisioning
		}
		if stores[owner] != nil {
			continue
		}
		a, found, err := r.life.NativeMailboxAssignment(owner.Mailbox)
		if err != nil || !found || a.Owner != owner || int64(len(d.Raw)) > a.Limits.MessageBytes {
			return sso.ErrNativeProvisioning
		}
		store, err := mailbox.OpenExisting(filepath.Join(a.Dir(r.stateDir), "mailbox"), owner, a.Limits, a.Source)
		if err != nil {
			return err
		}
		stores[owner] = store
		client, err := mailbox.NewClient(store, a.Address)
		if err != nil {
			return err
		}
		sources[owner] = client.MailSourceIdentity()
	}
	// Import authority does not require fresh DNS: already accepted bytes are
	// owed to the frozen owner. Refresh authorized TTLs before Claim sees them.
	return r.frozenAuthority(ctx, d, nil, func(current map[string]sso.NativeAssignment) error {
		for _, a := range current {
			if sources[a.Owner] != a.Source {
				return sso.ErrNativeProvisioning
			}
		}
		return r.holding.ImportTo(ctx, r.gatewayID(), id, folder, func(owner mailbox.Owner) (*mailbox.Store, error) {
			store := stores[owner]
			if store == nil {
				return nil, ingress.ErrRoute
			}
			return store, nil
		})
	})
}
