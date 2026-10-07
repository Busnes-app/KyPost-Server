package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// Continuous Cloudflare receiving: docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md.
const (
	cfGateway   = "cloudflare-continuous"
	cfOriginEnv = "KYPOST_CLOUDFLARE_RECEIVING_ORIGIN"
	cfInterval  = 30 * time.Second
	cfResign    = time.Hour
	// cfOldestWarning flags mail waiting in R2 longer than an hour: well past
	// 120 pickup cycles, so something (scanner, domain proof, capacity,
	// fencing) needs the operator, and far inside the 14-day table age.
	cfOldestWarning = time.Hour
	// cfFetches bounds provider downloads per cycle; the rest waits in R2.
	cfFetches  = 100
	cfTakeover = "move-receiving-here"
)

var errCFStop = errors.New("the receiving store is full (waiting or quarantined mail); new mail waits in R2 until imports finish or quarantined mail is released or discarded")

// cfOrigin is the operator's explicit profile selection; "" means off.
func cfOrigin() (string, error) {
	value := strings.TrimSpace(os.Getenv(cfOriginEnv))
	if value == "" {
		return "", nil
	}
	return cloudflareOrigin(value)
}

type cfLoop struct {
	r      *receivingRuntime
	keys   cfreceiving.Keys
	db     *cfreceiving.DB
	client cfreceiving.Client
	now    func() time.Time
	// crash is a test-only hook at each durable boundary; nil in production.
	crash func(point string) error
	// workerRevision is the Worker's stored revision; -1 until read.
	workerRevision int64
	// backoff holds keys refused for a transient reason (domain proof,
	// admission, scanner) so they neither refetch every cycle nor use the
	// per-cycle fetch budget ahead of newer mail.
	backoff map[string]cfBackoff
	status  cfreceiving.Status
}

func newCFLoop(r *receivingRuntime, db *cfreceiving.DB, origin string) *cfLoop {
	return &cfLoop{r: r, db: db, keys: cfreceiving.Keys{Dir: config.SecretDir()}, client: cfreceiving.Client{Origin: origin, HTTP: cfreceiving.NewHTTPClient()},
		now: time.Now, workerRevision: -1, backoff: map[string]cfBackoff{}}
}

type cfBackoff struct {
	until time.Time
	delay time.Duration
}

// defer doubles a key's wait from one minute to at most an hour.
func (l *cfLoop) postpone(key string) {
	b := l.backoff[key]
	b.delay = min(max(2*b.delay, time.Minute), time.Hour)
	b.until = l.now().Add(b.delay)
	l.backoff[key] = b
}

func (l *cfLoop) hit(point string) error {
	if l.crash != nil {
		return l.crash(point)
	}
	return nil
}

func cfLog(action, result, correlation string) {
	slog.Info("cloudflare continuous receiving", "actor", cfGateway, "task_id", "native-receiving", "action", action, "target", "cloudflare-worker", "result", result, "correlation_id", correlation)
}

// startCloudflareReceiving runs publish and pickup in the daemon when the
// operator selected the profile. A fenced or uninitialized instance idles and
// reports it; it never contacts the Worker.
func startCloudflareReceiving(ctx context.Context, d runDeps) (<-chan struct{}, error) {
	done := make(chan struct{})
	origin, err := cfOrigin()
	if err != nil || origin == "" {
		close(done)
		return done, err
	}
	spam, err := receivingRspamdEnabled()
	switch {
	case err != nil:
		return nil, err
	case !d.nativeReceiving:
		return nil, errors.New(cfOriginEnv + " requires KYPOST_NATIVE_RECEIVING=true")
	case os.Getenv("KYPOST_NATIVE_RECEIVER") == "true":
		return nil, errors.New(cfOriginEnv + " and the bundled receiver are separate profiles; enable only one")
	case !spam:
		return nil, errors.New(cfOriginEnv + " requires KYPOST_RECEIVING_RSPAMD=true and the qualified local sidecar")
	}
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return nil, err
	}
	r.gateway = cfGateway
	db, err := cfreceiving.Open(filepath.Join(r.stateDir, "receiving"))
	if err != nil {
		_ = r.holding.Close()
		return nil, err
	}
	l := newCFLoop(r, db, origin)
	go func() {
		defer close(done)
		defer r.holding.Close()
		defer db.Close()
		var last time.Time
		var seen string
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			mark := cfChangeMark(r)
			if time.Since(last) >= cfInterval || mark != seen {
				last, seen = time.Now(), mark
				if err := l.cycle(ctx); err != nil && ctx.Err() == nil {
					d.logger.Error("cloudflare receiving cycle deferred; provider copies retained", "error", err.Error())
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

// cfChangeMark changes when the address ledger or the sender block list is
// rewritten, so either publishes within one tick. The domain file is
// rewritten by every proof, so it is not watched.
func cfChangeMark(r *receivingRuntime) string {
	mark := ""
	for _, path := range []string{filepath.Join(r.configDir, "native-provisioning.json"), filepath.Join(r.stateDir, "receiving", ingress.BlocksFile)} {
		if info, err := os.Stat(path); err == nil {
			mark += info.ModTime().String() + strconv.FormatInt(info.Size(), 10)
			// Both files publish by rename: a new inode even within one mtime tick.
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				mark += "/" + strconv.FormatUint(st.Ino, 10)
			}
		}
		mark += "|"
	}
	return mark
}

// cycle publishes and picks up once, then saves status.
func (l *cfLoop) cycle(ctx context.Context) error {
	cur, _, live, err := l.keys.Load()
	l.status.Epoch, l.status.State, l.status.Detail = cur.Epoch, "running", ""
	switch {
	case err != nil:
		l.status.State, l.status.Detail = "error", err.Error()
	case !live:
		l.status.State, l.status.Detail = "fenced", "this instance does not hold Cloudflare receiving (restored, or another instance took over); confirm a takeover to move receiving here"
	default:
		err = l.publish(ctx, cur)
		if err != nil && !errors.Is(err, cfreceiving.ErrUnauthorized) {
			l.status.State, l.status.Detail = "error", "publish: "+err.Error()
			err = nil
		}
		if err == nil {
			err = l.pickup(ctx, cur)
		}
		if errors.Is(err, cfreceiving.ErrUnauthorized) {
			err = l.refused(ctx, cur)
		} else if err != nil {
			l.status.State, l.status.Detail = "error", "pickup: "+err.Error()
		}
	}
	l.status.UpdatedAt = l.now().UnixMilli()
	if saveErr := l.db.SaveStatus(context.WithoutCancel(ctx), l.status); err == nil {
		err = saveErr
	}
	return err
}

// refused handles a 401 for used. Under the credential lock: a rotation that
// landed meanwhile is not a fence; an interrupted on-demand rotation is
// confirmed by its own bearer; otherwise another instance took over and this
// one stops for good until an operator confirms a takeover.
func (l *cfLoop) refused(ctx context.Context, used cfreceiving.Material) error {
	release, err := l.keys.Lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	cur, pending, live, err := l.keys.Load()
	if err != nil || !live || cur.Token != used.Token {
		return err
	}
	if pending != nil {
		if ok, err := l.client.Authenticated(ctx, pending.Token); err != nil || ok {
			if ok {
				err = l.keys.Promote(*pending)
			}
			return err
		}
	}
	if err := l.keys.Fence(); err != nil {
		return err
	}
	l.status.State, l.status.Detail = "fenced", "the Worker refused this instance's credentials: another instance took over receiving; this one has stopped"
	cfLog("fence", "fenced", "credentials")
	return cfreceiving.ErrUnauthorized
}

// blockedSenders is the block list in force, in the Worker's form. An
// unreadable list fails the publish: the Worker keeps its last table rather
// than losing blocks.
func (l *cfLoop) blockedSenders() ([]cfreceiving.Block, error) {
	list, err := ingress.NewBlocks(filepath.Join(l.r.stateDir, "receiving")).List(l.now())
	blocks := make([]cfreceiving.Block, 0, len(list))
	for _, b := range list {
		w := cfreceiving.Block{Until: b.Until}
		if b.Kind == "domain" {
			w.Domain = b.Value
		} else {
			w.Address = b.Value
		}
		blocks = append(blocks, w)
	}
	return blocks, err
}

// publish signs and installs the table when it changed, when the Worker holds
// a newer revision than ours, or hourly.
func (l *cfLoop) publish(ctx context.Context, cur cfreceiving.Material) error {
	now := l.now().UnixMilli()
	if l.workerRevision < 0 {
		rev, err := l.client.Revision(ctx, cur.Token)
		if err != nil {
			return err
		}
		l.workerRevision = rev
	}
	routes, err := l.buildRoutes(ctx)
	if err != nil {
		return err
	}
	// An unreadable block list must not stop routes: the Worker refuses all
	// mail once its table is 14 days old. Keep the last installed blocks
	// (none if never published) and report the error.
	blocks, blockErr := l.blockedSenders()
	if blockErr != nil {
		if blocks, err = l.db.InstalledBlocks(ctx); err != nil {
			return err
		}
		blockErr = fmt.Errorf("%w; publishing routes with the last published blocks", blockErr)
		slog.Error("cloudflare sender blocks unreadable", "actor", cfGateway, "task_id", "native-receiving", "action", "publish", "target", "sender-blocks", "result", "previous-blocks-kept", "correlation_id", "sender-blocks", "error", blockErr.Error())
	}
	digest := cfreceiving.Digest(routes, blocks)
	installed, installedDigest, at, err := l.db.LastInstalled(ctx)
	if err != nil {
		return err
	}
	l.status.LastRevision, l.status.LastPublishAt = installed, at
	if installed >= l.workerRevision && installedDigest == digest && now-at < cfResign.Milliseconds() {
		return blockErr
	}
	recorded, err := l.db.LastRevision(ctx)
	if err != nil {
		return err
	}
	rev := max(now, recorded+1, l.workerRevision+1)
	if rev > now+4*time.Minute.Milliseconds() {
		return errors.New("the Worker's routing revision is ahead of this host's clock; correct the clock")
	}
	body, err := cfreceiving.SignTable(cur, rev, now, routes, blocks)
	if err != nil {
		return err
	}
	if err := l.db.Record(ctx, rev, now, digest, routes, blocks); err != nil {
		return err
	}
	if err := l.hit("recorded"); err != nil {
		return err
	}
	if err := l.client.PutRoutes(ctx, cur.Token, body); err != nil {
		if errors.Is(err, cfreceiving.ErrConflict) {
			l.workerRevision = -1
		}
		return err
	}
	l.workerRevision = rev
	l.status.LastRevision, l.status.LastPublishAt = rev, now
	slog.Info("cloudflare routing published", "actor", cfGateway, "task_id", "native-receiving", "action", "publish", "target", "cloudflare-worker", "result", "installed", "revision", strconv.FormatInt(rev, 10), "correlation_id", digest[:16])
	if err := l.db.Installed(ctx, rev, now); err != nil {
		return err
	}
	return blockErr
}

// buildRoutes lists admitted active addresses on proven domains. A failed
// proof or admission never prunes: such addresses keep the route the Worker
// already has at the same generation. Only durable ledger changes (release,
// disable, reassign, offboarding, domain removal) remove a route.
func (l *cfLoop) buildRoutes(ctx context.Context) ([]cfreceiving.Route, error) {
	r := l.r
	set, err := r.domains.ReadSet()
	if err != nil {
		return nil, err
	}
	proofs := map[string]sso.NativeDomain{}
	for name, d := range set.Domains {
		if !d.Established {
			continue
		}
		if proof, err := r.domains.VerifyDomain(ctx, name); err == nil {
			proofs[name] = proof
		}
	}
	addresses, err := r.life.NativeAddresses()
	if err != nil {
		return nil, err
	}
	previous := map[string]cfreceiving.Route{}
	if installed, _, _, err := l.db.LastInstalled(ctx); err != nil {
		return nil, err
	} else if installed > 0 {
		if previous, _, err = l.db.Routes(ctx, installed); err != nil {
			return nil, err
		}
	}
	byMailbox := map[string][]string{}
	for address, x := range addresses {
		if x.State == "active" && set.Domains[sso.AddressDomain(address)].Established && cfreceiving.ValidAddress(address) {
			byMailbox[x.Mailbox] = append(byMailbox[x.Mailbox], address)
		}
	}
	routes := []cfreceiving.Route{}
	// ponytail: one admission per mailbox per cycle; fine for a household or
	// small office. Past a few hundred mailboxes, admit once per directory
	// revision instead.
	for _, id := range slices.Sorted(maps.Keys(byMailbox)) {
		fresh, freshProofs := []string{}, []sso.NativeDomain{}
		for _, address := range byMailbox[id] {
			if proof, ok := proofs[sso.AddressDomain(address)]; ok {
				fresh = append(fresh, address)
				if !slices.Contains(freshProofs, proof) {
					freshProofs = append(freshProofs, proof)
				}
			}
		}
		added := map[string]bool{}
		if len(fresh) > 0 {
			_ = r.withAuthority(ctx, []string{id}, fresh, freshProofs, func(current map[string]sso.NativeAssignment) error {
				a := current[id]
				now, err := r.life.NativeAddresses()
				if err != nil {
					return err
				}
				for _, address := range fresh {
					if x := now[address]; x.State == "active" && x.Mailbox == id {
						routes = append(routes, cfreceiving.Route{Address: address, Generation: x.Generation, MaxBytes: min(receivingLimits.MessageBytes, a.Limits.MessageBytes, cfreceiving.MaxMessageBytes), Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox})
						added[address] = true
					}
				}
				return nil
			})
		}
		for _, address := range byMailbox[id] {
			if p, ok := previous[address]; ok && !added[address] && p.Generation == addresses[address].Generation && p.Mailbox == id {
				routes = append(routes, p)
			}
		}
	}
	slices.SortFunc(routes, func(a, b cfreceiving.Route) int { return strings.Compare(a.Address, b.Address) })
	return routes, nil
}

// pickup lists the whole queue from the start (the listing is not a durable
// cursor), handles each key against the ledger and holding store, then
// deletes provider copies the holding store has committed.
func (l *cfLoop) pickup(ctx context.Context, cur cfreceiving.Material) error {
	l.importOwed(ctx)
	listed := map[string]bool{}
	var oldest int64
	waiting, fetches := 0, 0
	stopped := false
	for after := ""; ; {
		items, truncated, err := l.client.List(ctx, cur.Token, after)
		if err == nil && truncated && len(items) == 0 {
			err = errors.New("the Worker returned an empty truncated listing page; retrying next cycle")
		}
		if err != nil {
			return err
		}
		for _, it := range items {
			after, listed[it.Key] = it.Key, true
			settled := false
			if !stopped && fetches < cfFetches {
				var fetched bool
				settled, fetched, err = l.item(ctx, cur, it)
				if fetched {
					fetches++
				}
				switch {
				case errors.Is(err, cfreceiving.ErrUnauthorized):
					return err
				case errors.Is(err, errCFStop):
					stopped = true
					l.status.State, l.status.Detail = "error", errCFStop.Error()
				case err != nil:
					slog.Warn("cloudflare pickup deferred; provider copy retained", "actor", cfGateway, "task_id", "native-receiving", "action", "pickup", "target", "holding-store", "result", "deferred", "error", err.Error(), "correlation_id", it.Key)
				}
			}
			if !settled {
				waiting++
				if t := cfreceiving.KeyTime(it.Key); oldest == 0 || t < oldest {
					oldest = t
				}
			}
		}
		if !truncated {
			break
		}
	}
	now := l.now().UnixMilli()
	warn := oldest > 0 && now-oldest > cfOldestWarning.Milliseconds()
	if warn && !l.status.OldestWarning {
		slog.Warn("cloudflare mail waiting over an hour in R2; check scanner, domain proof, capacity and status", "actor", cfGateway, "task_id", "native-receiving", "action", "pickup", "target", "cloudflare-worker", "result", "delayed", "correlation_id", "oldest-unpicked")
	}
	l.status.Waiting, l.status.OldestUnpickedAt, l.status.OldestWarning, l.status.LastPickupAt = waiting, oldest, warn, now
	for key := range l.backoff {
		if !listed[key] {
			delete(l.backoff, key)
		}
	}
	return l.deleteCommitted(ctx, cur, listed)
}

// item returns settled once the key no longer waits for pickup: its local
// record is terminal (provider delete owed) or it is refused (kept for the
// operator, counted separately).
func (l *cfLoop) item(ctx context.Context, cur cfreceiving.Material, it cfreceiving.Item) (settled, fetched bool, err error) {
	entry, found, err := l.db.Entry(ctx, it.Key)
	if err != nil || found && entry.State == "refused" {
		return err == nil, false, err
	}
	d, err := l.r.holding.Get(ctx, cfGateway, it.Key)
	absent := errors.Is(err, sql.ErrNoRows)
	if absent {
		d = ingress.Delivery{} // Get reports an absent ID as "archived"
	} else if err != nil {
		return false, false, err
	}
	if !absent && d.State != "staged" && d.Digest != it.Digest || found && entry.Digest != it.Digest {
		// The provider object differs from this key's local record.
		return true, false, l.refuse(ctx, it.Key, it.Digest)
	}
	switch d.State {
	case "archived", "quarantined":
		state := "quarantined"
		if d.Disposition == "imported" {
			state = "imported"
		}
		return true, false, l.db.SetEntry(ctx, it.Key, d.Digest, state)
	case "pending":
		settled, err = l.deliver(ctx, it.Key, d.Digest, found && entry.State == "junk")
		return settled, false, err
	}
	if l.now().Before(l.backoff[it.Key].until) {
		return false, false, nil
	}
	if it.Size > receivingLimits.MessageBytes {
		return true, false, l.refuse(ctx, it.Key, it.Digest)
	}
	env, raw, err := l.client.Fetch(ctx, cur.Token, it.Key, receivingLimits.MessageBytes)
	if errors.Is(err, cfreceiving.ErrInvalid) || err == nil && (env.Digest != it.Digest || !validEnvelopeSender(env.Sender)) {
		return true, true, l.refuse(ctx, it.Key, it.Digest)
	}
	if err == nil {
		err = l.hit("fetched")
	}
	if err != nil {
		return false, true, err
	}
	settled, err = l.capture(ctx, env, raw, absent)
	if err != nil && !errors.Is(err, errCFStop) {
		l.postpone(it.Key)
	}
	return settled, true, err
}

// validEnvelopeSender is the holding store's sender rule; the Worker admits
// some forms it refuses, and those stay at the provider as refused.
func validEnvelopeSender(sender string) bool {
	if sender == "" {
		return true
	}
	a, err := mail.ParseAddress(sender)
	return err == nil && a.Name == "" && a.Address == sender && len(sender) <= 320
}

// refuse keeps a provider object this instance cannot hold; it is never
// deleted and stays visible in status for the operator.
func (l *cfLoop) refuse(ctx context.Context, key, digest string) error {
	cfLog("refuse", "retained-at-provider", key)
	return l.db.SetEntry(ctx, key, digest, "refused")
}

// frozen resolves the envelope's (address, generation, tableRevision) to the
// owner this instance published; false when it cannot.
func (l *cfLoop) frozen(ctx context.Context, env cfreceiving.Envelope) (cfreceiving.Route, bool, error) {
	routes, known, err := l.db.Routes(ctx, env.TableRevision)
	route, listed := routes[env.Recipient]
	return route, err == nil && known && listed && route.Generation == env.Generation && env.Size <= route.MaxBytes, err
}

func (l *cfLoop) capture(ctx context.Context, env cfreceiving.Envelope, raw []byte, absent bool) (bool, error) {
	r := l.r
	route, known, err := l.frozen(ctx, env)
	if err != nil {
		return false, err
	}
	if !known {
		// Restored without that revision, or not ours: never guess an owner.
		return l.quarantine(ctx, env, raw, ingress.Binding{Address: env.Recipient, Generation: env.Generation})
	}
	binding := ingress.Binding{Address: route.Address, Issuer: route.Issuer, Subject: route.Subject, Mailbox: route.Mailbox, Generation: route.Generation}
	owner := mailbox.Owner{Issuer: route.Issuer, Subject: route.Subject, Mailbox: route.Mailbox}
	if absent {
		err := r.bindExpected(ctx, env.ID, env.Sender, env.Recipient, func(a sso.NativeAssignment, x sso.NativeAddress) bool {
			return a.Owner == owner && x.Generation == route.Generation
		})
		if err != nil {
			return l.unbound(ctx, env, raw, binding, err)
		}
		if err := l.hit("bound"); err != nil {
			return false, err
		}
	}
	entry, _, err := l.db.Entry(ctx, env.ID)
	if err != nil {
		return false, err
	}
	junk := entry.State == "junk"
	d, err := r.holding.Get(ctx, cfGateway, env.ID)
	if err != nil {
		return false, err
	}
	if !junk {
		var verdict *receivingCommandError
		if err := scanReceivingSpam(ctx, raw, d, "", "", receivingRspamdURL); errors.As(err, &verdict) && verdict.code == 4 {
			// Cloudflare already accepted it, so it cannot bounce: file it
			// to Junk, never drop it. Durable before the bytes are.
			if err := l.db.SetEntry(ctx, env.ID, env.Digest, "junk"); err != nil {
				return false, err
			}
			junk = true
			if err := l.hit("junk"); err != nil {
				return false, err
			}
		} else if err != nil {
			return false, err // soft reject or scanner failure: retry later
		}
	}
	proofs, err := r.verifyDomains(ctx, []string{env.Recipient})
	if err == nil {
		err = r.commitAccept(ctx, d, env.Sender, raw, proofs)
	}
	if err != nil {
		return l.unbound(ctx, env, raw, binding, err)
	}
	if err := l.hit("accepted"); err != nil {
		return false, err
	}
	return l.deliver(ctx, env.ID, env.Digest, junk)
}

// unbound sorts a refused bind or accept: a durable authority change, or a
// message over its mailbox's per-message limit, quarantines with bytes; a
// full receiving store stops fetching; anything else (lapsed proof, DNS,
// admission) leaves the mail waiting in R2 with a backoff.
func (l *cfLoop) unbound(ctx context.Context, env cfreceiving.Envelope, raw []byte, b ingress.Binding, err error) (bool, error) {
	switch {
	case errors.Is(err, ingress.ErrRoute) || errors.Is(err, errMailboxMessageLimit) || l.r.bindingsRetired(ctx, []ingress.Binding{b}):
		return l.quarantine(ctx, env, raw, b)
	case errors.Is(err, ingress.ErrCapacity):
		return false, errCFStop
	}
	return false, err
}

func (l *cfLoop) quarantine(ctx context.Context, env cfreceiving.Envelope, raw []byte, b ingress.Binding) (bool, error) {
	if err := l.r.holding.Quarantine(ctx, cfGateway, env.ID, env.Sender, b, raw); err != nil {
		if errors.Is(err, ingress.ErrCapacity) {
			return false, errCFStop
		}
		return false, err
	}
	if err := l.hit("quarantined"); err != nil {
		return false, err
	}
	cfLog("quarantine", "quarantined", env.ID)
	return true, l.db.SetEntry(ctx, env.ID, env.Digest, "quarantined")
}

// deliver imports a pending delivery to INBOX, or Junk after a reject verdict.
func (l *cfLoop) deliver(ctx context.Context, key, digest string, junk bool) (bool, error) {
	folder := "INBOX"
	if junk {
		folder = "Junk"
	}
	err := l.r.importDeliveryTo(ctx, key, folder)
	if errors.Is(err, ingress.ErrRoute) {
		if d, getErr := l.r.holding.Get(ctx, cfGateway, key); getErr == nil && d.State == "quarantined" {
			return true, l.db.SetEntry(ctx, key, digest, "quarantined")
		}
	}
	if err == nil {
		err = l.hit("imported")
	}
	if err != nil {
		return false, err
	}
	return true, l.db.SetEntry(ctx, key, digest, "imported")
}

// importOwed imports pending deliveries even when their provider copy has
// gone (an administrator deleted it); listing alone would never see them.
func (l *cfLoop) importOwed(ctx context.Context) {
	for after := int64(0); ctx.Err() == nil; {
		rows, err := l.r.holding.List(ctx, cfGateway, after, 100)
		if err != nil || len(rows) == 0 {
			return
		}
		for _, row := range rows {
			after = row.Sequence
			if row.State != "pending" {
				continue
			}
			entry, _, err := l.db.Entry(ctx, row.ID)
			d, getErr := l.r.holding.Get(ctx, cfGateway, row.ID)
			if err == nil && getErr == nil {
				_, err = l.deliver(ctx, row.ID, d.Digest, entry.State == "junk")
			}
			if err != nil {
				slog.Warn("cloudflare import deferred; holding bytes retained", "actor", cfGateway, "task_id", "native-receiving", "action", "import", "target", "holding-store", "result", "deferred", "error", err.Error(), "correlation_id", row.ID)
			}
		}
	}
}

// deleteCommitted removes provider copies of deliveries the holding store
// has committed, by the local digest. The holding store decides, not the
// ledger: after a restore the ledger may name deliveries the restored store
// never committed, and those stay at the provider to be picked up again.
func (l *cfLoop) deleteCommitted(ctx context.Context, cur cfreceiving.Material, listed map[string]bool) error {
	entries, err := l.db.Entries(ctx, "imported", "quarantined", "refused", "junk")
	if err != nil {
		return err
	}
	for _, e := range entries {
		d, err := l.r.holding.Get(ctx, cfGateway, e.Key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		committed := err == nil && d.Digest == e.Digest && (d.State == "archived" || d.State == "quarantined")
		var stale bool
		switch e.State {
		case "refused":
			stale = !listed[e.Key] // gone from the provider
		case "junk":
			// Still owed only while pending here or fetchable there.
			stale = !listed[e.Key] && (err != nil || d.State != "pending")
		default:
			// A delete-owed row the holding store has not committed is
			// picked up again.
			stale = !committed
		}
		if stale {
			if err := l.db.RemoveEntry(ctx, e.Key); err != nil {
				return err
			}
		}
		if e.State == "refused" || e.State == "junk" || !committed {
			continue
		}
		if err := l.hit("delete"); err != nil {
			return err
		}
		switch err := l.client.Delete(ctx, cur.Token, e.Key, e.Digest); {
		case err == nil:
			cfLog("delete", "deleted", e.Key)
			if err := l.db.RemoveEntry(ctx, e.Key); err != nil {
				return err
			}
		case errors.Is(err, cfreceiving.ErrUnauthorized):
			return err
		case errors.Is(err, cfreceiving.ErrConflict):
			if err := l.refuse(ctx, e.Key, e.Digest); err != nil {
				return err
			}
		default:
			slog.Warn("cloudflare provider delete deferred; retried from the ledger", "actor", cfGateway, "task_id", "native-receiving", "action", "delete", "target", "cloudflare-worker", "result", "deferred", "error", err.Error(), "correlation_id", e.Key)
		}
	}
	return nil
}

// bindingsRetired reports a durable ledger change for a frozen binding: the
// address moved, is inactive, or has a newer generation. Unreadable state
// proves nothing.
func (r *receivingRuntime) bindingsRetired(ctx context.Context, bindings []ingress.Binding) bool {
	release, err := r.life.LockDirectoryContext(ctx)
	if err != nil {
		return false
	}
	defer release()
	addresses, err := r.life.NativeAddresses()
	return err == nil && retiredBinding(addresses, bindings)
}

func retiredBinding(addresses map[string]sso.NativeAddress, bindings []ingress.Binding) bool {
	return slices.ContainsFunc(bindings, func(b ingress.Binding) bool {
		x := addresses[b.Address]
		return x.Mailbox != b.Mailbox || x.State != "active" || x.Generation != b.Generation
	})
}

// cfRotate moves the Worker to fresh credentials. Pending material is
// persisted host-locally before it is sent, so a retry resends the same
// material; a lost response is confirmed by the pending bearer itself.
// takeover allows a fenced (restored) instance to rotate away from the
// credentials it was restored with.
func cfRotate(ctx context.Context, keys cfreceiving.Keys, client cfreceiving.Client, takeover bool, crash func(string) error) error {
	release, err := keys.Lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	cur, pending, live, err := keys.Load()
	if err != nil {
		return err
	}
	if !live && !takeover {
		return errors.New("this instance is fenced; only a confirmed takeover can rotate")
	}
	if pending == nil || pending.Epoch != cur.Epoch+1 {
		next, err := cfreceiving.NewMaterial(cur.Epoch + 1)
		if err == nil {
			err = keys.SavePending(next)
		}
		if err != nil {
			return err
		}
		pending = &next
	}
	if crash != nil {
		if err := crash("pending"); err != nil {
			return err
		}
	}
	body, err := cfreceiving.SignRotation(cur, *pending)
	if err != nil {
		return err
	}
	status, rotateErr := client.Rotate(ctx, cur.Token, body)
	if status != 204 {
		// Our own earlier success, a lost response, or another instance's
		// rotation: only the pending bearer can tell them apart.
		ok, err := client.Authenticated(ctx, pending.Token)
		if err != nil {
			return errors.New("rotation unconfirmed; the Worker is unreachable, so rerun the same command")
		}
		if !ok {
			if status == 401 || status == 403 || status == 409 {
				if err := keys.Fence(); err != nil {
					return err
				}
				cfLog("rotate", "fenced", "credentials")
				return errors.New("another instance rotated the Worker credentials first; this instance stays fenced and does not receive")
			}
			if rotateErr == nil {
				rotateErr = fmt.Errorf("the Worker refused the rotation (HTTP %d)", status)
			}
			return fmt.Errorf("%w; rerun the same command", rotateErr)
		}
	}
	if err := keys.Promote(*pending); err != nil {
		return err
	}
	cfLog("rotate", "installed", "epoch-"+strconv.FormatInt(pending.Epoch, 10))
	return nil
}

const cfUsage = "usage: receiving cloudflare init | rotate | takeover --confirm " + cfTakeover + " | status"

// runCloudflareContinuous is the operator CLI. init prints only the two
// Worker secrets (the bearer's SHA-256 and the public key); the bearer and
// signing key never leave SECRET_DIR.
func runCloudflareContinuous(args []string, output io.Writer) (result error) {
	action := args[0]
	defer func() {
		status := "committed"
		if result != nil {
			status = "refused"
		}
		cfLog(action, status, "operator:"+strconv.Itoa(os.Geteuid()))
	}()
	switch {
	case len(args) == 1 && (action == "init" || action == "rotate" || action == "status"):
	case len(args) == 3 && action == "takeover" && args[1] == "--confirm" && args[2] == cfTakeover:
	case action == "takeover":
		return errors.New("takeover moves Cloudflare receiving to this host and the current host stops receiving; repeat with --confirm " + cfTakeover)
	default:
		return errors.New(cfUsage)
	}
	// Another user would create credentials the daemon cannot read.
	info, err := os.Stat(config.StateDir())
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("run as the owner of %s: docker compose exec --user kypost kypost-server kypost-server receiving cloudflare %s", config.StateDir(), action)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	keys := cfreceiving.Keys{Dir: config.SecretDir()}
	switch action {
	case "init":
		release, err := keys.Lock(ctx)
		if err != nil {
			return err
		}
		defer release()
		m, err := keys.Init()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Deploy these two Worker secrets (wrangler secret put <NAME>), then set %s:\nPICKUP_TOKEN_SHA256=%s\nROUTING_PUBLIC_KEY=%s\n", cfOriginEnv, m.TokenSHA256(), m.PublicKey())
		return err
	case "status":
		s, err := cfreceiving.CurrentStatus(ctx, keys, filepath.Join(config.StateDir(), "receiving"))
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(s)
	}
	origin, err := cfOrigin()
	if err != nil || origin == "" {
		return errors.Join(errors.New("set "+cfOriginEnv+" to the Worker origin"), err)
	}
	// A held restore cannot receive, so it must not take receiving away from
	// the host that can.
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return err
	}
	_ = r.holding.Close()
	return cfRotate(ctx, keys, cfreceiving.Client{Origin: origin, HTTP: cfreceiving.NewHTTPClient()}, action == "takeover", nil)
}
