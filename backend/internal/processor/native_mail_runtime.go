package processor

import (
	"context"
	"path/filepath"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// EnableNativeMail is startup-only, before Start launches goroutines.
func (p *Poller) EnableNativeMail() { p.nativeMail = true }

func (p *Poller) nativeMailAssignment(ctx context.Context, userID string) (sso.NativeAssignment, bool, error) {
	u, err := p.users.Get(userID)
	if err != nil {
		return sso.NativeAssignment{}, false, err
	}
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		return sso.NativeAssignment{}, false, nil
	}
	settings := sso.NewStore(p.configDir).Load()
	if !p.nativeMail || !settings.Enabled {
		return sso.NativeAssignment{}, true, sso.ErrNativeProvisioning
	}
	a, err := sso.NewLifecycleStore(p.configDir).AdmitNativeMail(ctx, p.stateDir, settings.IssuerURL, userID, p.users)
	return a, true, err
}

func (p *Poller) mailClientForUser(userID string, modTime time.Time) (imapadapter.Client, error) {
	if p.users == nil {
		return p.userMailClient(userID, modTime), nil
	}
	a, native, err := p.nativeMailAssignment(p.lifetimeCtx(), userID)
	if err != nil {
		return nil, err
	}
	if !native {
		return p.userMailClient(userID, modTime), nil
	}
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if entry, ok := p.mailClients[userID]; ok && entry.source == a.Source {
		return entry.client, nil
	}
	client, err := mailbox.OpenClient(filepath.Join(p.userStateDir(userID), "mailbox"), a.Owner, a.Limits, a.Address, a.Source, func(ctx context.Context) error {
		current, _, err := p.nativeMailAssignment(ctx, userID)
		if err != nil {
			return err
		}
		if current.Source != a.Source {
			return sso.ErrNativeProvisioning
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.mailClients[userID] = &mailClientEntry{client: client, source: a.Source}
	return client, nil
}
