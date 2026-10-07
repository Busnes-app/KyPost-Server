package processor

import (
	"cmp"
	"context"
	"path/filepath"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// EnableNativeMail is startup-only, before Start launches goroutines.
func (p *Poller) EnableNativeMail() { p.nativeMail = true }

func (p *Poller) nativeMailAssignment(ctx context.Context, userID string) (sso.NativeAssignment, bool, error) {
	return p.nativeMailboxAssignment(ctx, userID, "")
}

// nativeMailboxAssignment admits one of the user's mailboxes ("" is the
// primary).
func (p *Poller) nativeMailboxAssignment(ctx context.Context, userID, mailboxID string) (sso.NativeAssignment, bool, error) {
	u, err := p.users.Get(userID)
	if err != nil {
		return sso.NativeAssignment{}, false, err
	}
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		if mailboxID != "" {
			return sso.NativeAssignment{}, false, sso.ErrNativeMailboxUnknown
		}
		return sso.NativeAssignment{}, false, nil
	}
	settings := sso.NewStore(p.configDir).Load()
	if !p.nativeMail || !settings.Enabled {
		return sso.NativeAssignment{}, true, sso.ErrNativeProvisioning
	}
	a, err := sso.NewLifecycleStore(p.configDir).AdmitNativeMailbox(ctx, p.stateDir, settings.IssuerURL, userID, cmp.Or(mailboxID, userID), p.users)
	return a, true, err
}

func (p *Poller) mailClientForUser(userID string, modTime time.Time) (imapadapter.Client, error) {
	client, _, err := p.mailClientForMailbox(userID, "", modTime)
	return client, err
}

// mailClientForMailbox returns the admitted mailbox's client ("" is the
// primary); clients are cached by mailbox ID, which for a primary is the user
// ID. The assignment is zero for an IMAP account.
func (p *Poller) mailClientForMailbox(userID, mailboxID string, modTime time.Time) (imapadapter.Client, sso.NativeAssignment, error) {
	if p.users == nil {
		return p.userMailClient(userID, modTime), sso.NativeAssignment{}, nil
	}
	a, native, err := p.nativeMailboxAssignment(p.lifetimeCtx(), userID, mailboxID)
	if err != nil {
		return nil, a, err
	}
	if !native {
		return p.userMailClient(userID, modTime), a, nil
	}
	key := a.Owner.Mailbox
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if entry, ok := p.mailClients[key]; ok && entry.source == a.Source {
		return entry.client, a, nil
	}
	client, err := mailbox.OpenClient(filepath.Join(a.Dir(p.stateDir), "mailbox"), a.Owner, a.Limits, a.Address, a.Source, func(ctx context.Context) error {
		current, _, err := p.nativeMailboxAssignment(ctx, userID, mailboxID)
		if err != nil {
			return err
		}
		if current.Source != a.Source {
			return sso.ErrNativeProvisioning
		}
		return nil
	})
	if err != nil {
		return nil, a, err
	}
	p.mailClients[key] = &mailClientEntry{client: client, source: a.Source}
	return client, a, nil
}

// mailboxStore opens an admitted extra mailbox's mail-only state, cached by
// mailbox ID; the caller admitted a this tick.
func (p *Poller) mailboxStore(a sso.NativeAssignment) (*state.Store, error) {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if st, ok := p.stores[a.Owner.Mailbox]; ok {
		return st, nil
	}
	st, err := state.OpenNative(a.Dir(p.stateDir), a.Source)
	if err != nil {
		return nil, err
	}
	p.stores[a.Owner.Mailbox] = st
	return st, nil
}

// mailboxCacheStore is userMailCacheStore for the polled mailbox; a is zero
// for an IMAP account and is the primary's assignment for a primary mailbox.
func (p *Poller) mailboxCacheStore(userID string, a sso.NativeAssignment) (*mailcache.Store, error) {
	if a.Owner.Mailbox == "" || a.Owner.Mailbox == userID {
		return p.userMailCacheStore(userID)
	}
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if st, ok := p.mailCaches[a.Owner.Mailbox]; ok {
		return st, nil
	}
	st, err := mailcache.New(a.Dir(p.stateDir))
	if err != nil {
		return nil, err
	}
	p.mailCaches[a.Owner.Mailbox] = st
	return st, nil
}
