package mailmsg

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// DKIM (RFC 6376) and DMARC (RFC 7489) do not require rotation; the 180-day
// recommendation follows M3AAWG's DKIM Key Rotation BCP (at least every six
// months). Rotation itself is always an administrator's action.
const (
	dkimRotationAge  = 180 * 24 * time.Hour
	dkimRetireWindow = 7 * 24 * time.Hour  // mail already signed by the old key is still checked
	dkimRemovableFor = 30 * 24 * time.Hour // how long "can now be removed from DNS" is shown
	dkimMaxRetired   = 4
)

var (
	ErrDKIMNotDirect   = errors.New("DKIM rotation applies to direct delivery only; with a relay, the provider signs")
	ErrDKIMUnknown     = errors.New("that domain is not a sending domain of the direct profile")
	ErrDKIMRetiring    = errors.New("finish the previous rotation first: the old DKIM key is still retiring, so keep its record published until its date")
	ErrDKIMNoNext      = errors.New("no new DKIM key is waiting; rotate the key first")
	ErrDKIMNotLive     = errors.New("the new DKIM record is not live yet")
	ErrDKIMNextChanged = errors.New("the waiting DKIM key changed during the check; check again")
)

// DKIMEvent describes one transition, for the "dkim rotation" log line. It
// names selectors only, never key material.
type DKIMEvent struct {
	Domain, OldSelector, NewSelector, Phase, Trigger string
}

// DKIMRotation runs key transitions on one sealed profile. Fence is the
// native-domain lock, taken before the relay file lock as the admin save path
// does. Every transition re-reads the file under both locks and is one
// atomic write through writeDomainRelay; Generation never changes.
type DKIMRotation struct {
	Path, KeyPath, Fence string
	Resolver             DNSResolver
}

func (r DKIMRotation) edit(ctx context.Context, apply func(c *DomainRelay) ([]DKIMEvent, error)) (DomainRelay, []DKIMEvent, error) {
	fence, err := fsutil.LockFileContext(ctx, r.Fence)
	if err != nil {
		return DomainRelay{}, nil, err
	}
	defer fence()
	release, err := fsutil.LockFileContext(ctx, r.Path)
	if err != nil {
		return DomainRelay{}, nil, err
	}
	defer release()
	prior, exists, err := ReadDomainRelay(r.Path, r.KeyPath)
	if err != nil {
		return DomainRelay{}, nil, err
	}
	if !exists || prior.Mode != "direct" {
		return prior, nil, ErrDKIMNotDirect
	}
	c := prior
	c.DKIM = slices.Clone(prior.DKIM)
	events, err := apply(&c)
	if err != nil || c.Equal(prior) {
		return prior, nil, err
	}
	slices.SortFunc(c.DKIM, compareDKIM)
	if c.Validate() != nil {
		return prior, nil, ErrDomainRelay
	}
	if err := writeDomainRelay(ctx, r.Path, r.KeyPath, c); err != nil {
		return prior, nil, err
	}
	return c, events, nil
}

func (r DKIMRotation) read() (DomainRelay, error) {
	c, exists, err := ReadDomainRelay(r.Path, r.KeyPath)
	if err == nil && (!exists || c.Mode != "direct") {
		err = ErrDKIMNotDirect
	}
	return c, err
}

// Rotate prepares a next key for domain. The active key keeps signing until
// the administrator's Confirm sees the next record live. A waiting next key is returned
// unchanged, so a repeated call writes nothing.
func (r DKIMRotation) Rotate(ctx context.Context, now time.Time, domain string) (DomainRelay, DKIMKey, []DKIMEvent, error) {
	c, events, err := r.edit(ctx, func(c *DomainRelay) ([]DKIMEvent, error) {
		if !c.Sends(domain) {
			return nil, ErrDKIMUnknown
		}
		if _, ok := c.dkimKey(domain, DKIMNext); ok {
			return nil, nil
		}
		if _, ok := c.dkimKey(domain, DKIMRetiring); ok {
			return nil, ErrDKIMRetiring
		}
		e, err := c.prepareNext(domain, now, "manual")
		return []DKIMEvent{e}, err
	})
	next, _ := c.dkimKey(domain, DKIMNext)
	return c, next, events, err
}

// RotateNow is the emergency path: a fresh key signs at once, before its
// record can be live, and the old key is dropped without a retire window
// because it is assumed compromised. It is never refused for a pending
// rotation.
func (r DKIMRotation) RotateNow(ctx context.Context, now time.Time, domain string) (DomainRelay, []DKIMEvent, error) {
	return r.edit(ctx, func(c *DomainRelay) ([]DKIMEvent, error) {
		if !c.Sends(domain) {
			return nil, ErrDKIMUnknown
		}
		old, _ := c.dkimKey(domain, DKIMActive)
		fresh, err := c.newDKIMKey(domain, DKIMActive, now)
		if err != nil {
			return nil, ErrDomainRelay
		}
		c.DKIM = slices.DeleteFunc(c.DKIM, func(k DKIMKey) bool { return k.Domain == domain && (k.State == DKIMNext || k.State == DKIMActive) })
		if old.Selector != "" {
			c.retire(old, DKIMRemovable, now, now)
		}
		c.DKIM = append(c.DKIM, fresh)
		return []DKIMEvent{{Domain: domain, OldSelector: old.Selector, NewSelector: fresh.Selector, Phase: "emergency", Trigger: "emergency"}}, nil
	})
}

// Confirm looks up domain's next record and switches to it when it is live.
// DNS runs before the locks; the switch re-checks that the same key is still
// waiting. A record that is not live returns its check and ErrDKIMNotLive.
func (r DKIMRotation) Confirm(ctx context.Context, now time.Time, domain string) (DomainRelay, DNSCheck, []DKIMEvent, error) {
	c, err := r.read()
	if err != nil {
		return DomainRelay{}, DNSCheck{}, nil, err
	}
	if !c.Sends(domain) {
		return c, DNSCheck{}, nil, ErrDKIMUnknown
	}
	next, ok := c.dkimKey(domain, DKIMNext)
	if !ok {
		return c, DNSCheck{}, nil, ErrDKIMNoNext
	}
	check := r.lookup(ctx, next)
	if !live(check) {
		return c, check, nil, ErrDKIMNotLive
	}
	c, events, err := r.edit(ctx, func(c *DomainRelay) ([]DKIMEvent, error) {
		current, ok := c.dkimKey(domain, DKIMNext)
		if !ok {
			if active, _ := c.dkimKey(domain, DKIMActive); active.Selector == next.Selector {
				return nil, nil // a concurrent confirm switched it
			}
			return nil, ErrDKIMNextChanged
		}
		if !current.equal(next) {
			return nil, ErrDKIMNextChanged
		}
		return []DKIMEvent{c.switchNext(domain, now)}, nil
	})
	return c, check, events, err
}

// Tick advances the windows of rotations an administrator started: a
// retiring key whose window has passed loses its private key and becomes
// removable, and a removable entry is dropped 30 days later. It never
// creates or switches a key and makes no DNS lookup; at 180 days the admin
// screen only recommends rotating. A relay profile is left alone. Repeating
// a tick with the same state and now writes nothing.
func (r DKIMRotation) Tick(ctx context.Context, now time.Time) (DomainRelay, []DKIMEvent, error) {
	c, events, err := r.edit(ctx, func(c *DomainRelay) ([]DKIMEvent, error) {
		var events []DKIMEvent
		for i := range c.DKIM {
			if k := c.DKIM[i]; k.State == DKIMRetiring && !now.Before(k.KeepPublishedUntil) {
				c.DKIM[i].State, c.DKIM[i].Private = DKIMRemovable, nil
				events = append(events, DKIMEvent{Domain: k.Domain, OldSelector: k.Selector, Phase: "destroy", Trigger: "window"})
			}
		}
		c.DKIM = slices.DeleteFunc(c.DKIM, func(k DKIMKey) bool {
			drop := k.State == DKIMRemovable && !now.Before(k.KeepPublishedUntil.Add(dkimRemovableFor))
			if drop {
				events = append(events, DKIMEvent{Domain: k.Domain, OldSelector: k.Selector, Phase: "drop", Trigger: "window"})
			}
			return drop
		})
		return events, nil
	})
	if errors.Is(err, ErrDKIMNotDirect) {
		err = nil
	}
	return c, events, err
}

func (r DKIMRotation) lookup(ctx context.Context, k DKIMKey) DNSCheck {
	ctx, cancel := context.WithTimeout(ctx, deliverabilityBudget)
	defer cancel()
	return checkDKIMKey(dnsQuery{ctx: ctx, r: r.Resolver}, k)
}

// live: the record carries the key and verifies a test signature (t=y included).
func live(check DNSCheck) bool { return check.Status == "pass" || check.Status == "warn" }

func (c *DomainRelay) prepareNext(domain string, now time.Time, trigger string) (DKIMEvent, error) {
	k, err := c.newDKIMKey(domain, DKIMNext, now)
	if err != nil {
		return DKIMEvent{}, ErrDomainRelay
	}
	active, _ := c.dkimKey(domain, DKIMActive)
	c.DKIM = append(c.DKIM, k)
	return DKIMEvent{Domain: domain, OldSelector: active.Selector, NewSelector: k.Selector, Phase: "prepare", Trigger: trigger}, nil
}

// switchNext makes domain's next key active and starts the old key's retire window.
func (c *DomainRelay) switchNext(domain string, now time.Time) DKIMEvent {
	old, _ := c.dkimKey(domain, DKIMActive)
	c.DKIM = slices.DeleteFunc(c.DKIM, func(k DKIMKey) bool { return k.Domain == domain && k.State == DKIMActive })
	var selector string
	for i, k := range c.DKIM {
		if k.Domain == domain && k.State == DKIMNext {
			c.DKIM[i].State, c.DKIM[i].ActivatedAt = DKIMActive, at(now)
			selector = k.Selector
		}
	}
	c.retire(old, DKIMRetiring, now, now.Add(dkimRetireWindow))
	return DKIMEvent{Domain: domain, OldSelector: old.Selector, NewSelector: selector, Phase: "switch", Trigger: "manual"}
}

// retire adds k as retiring or removable, dropping the oldest removable
// entry of its domain when the retired list is full.
func (c *DomainRelay) retire(k DKIMKey, state string, now, until time.Time) {
	k.State, k.RetiredAt, k.KeepPublishedUntil = state, at(now), at(until)
	if state == DKIMRemovable {
		k.Private = nil
	}
	old := slices.DeleteFunc(slices.Clone(c.DKIM), func(o DKIMKey) bool {
		return o.Domain != k.Domain || o.State != DKIMRetiring && o.State != DKIMRemovable
	})
	if len(old) >= dkimMaxRetired {
		var oldest DKIMKey
		for _, o := range old {
			if o.State == DKIMRemovable && (oldest.Selector == "" || o.RetiredAt.Before(oldest.RetiredAt)) {
				oldest = o
			}
		}
		c.DKIM = slices.DeleteFunc(c.DKIM, func(o DKIMKey) bool { return o.Domain == k.Domain && o.Selector == oldest.Selector })
	}
	c.DKIM = append(c.DKIM, k)
}
