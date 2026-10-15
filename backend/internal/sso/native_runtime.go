package sso

import (
	"cmp"
	"context"
	"fmt"

	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// ErrNativeMailDisabled refuses a native account while KYPOST_NATIVE_MAIL is off.
var ErrNativeMailDisabled = fmt.Errorf("native mailbox mode is disabled; enable KYPOST_NATIVE_MAIL explicitly: %w", ErrNativeProvisioning)

// AdmitUserMailbox is the native-mail admission the api and daemon share;
// mailboxID "" is the primary. native is true for every native account, so a
// refused one (mode off, SSO off, authority stale) never falls back to IMAP.
// An account that is not native has no extra mailboxes.
// It uses only exported sso API so it can move to internal/native as is.
func AdmitUserMailbox(ctx context.Context, nativeMode bool, settings *Store, life *LifecycleStore, accounts *users.Store, stateRoot, userID, mailboxID string) (a NativeAssignment, native bool, err error) {
	u, err := accounts.Get(userID)
	if err != nil {
		return NativeAssignment{}, false, err
	}
	if u.NativeMailboxIssuer == "" && u.NativeMailboxSource == "" {
		if mailboxID != "" {
			return NativeAssignment{}, false, ErrNativeMailboxUnknown
		}
		return NativeAssignment{}, false, nil
	}
	if !nativeMode {
		return NativeAssignment{}, true, ErrNativeMailDisabled
	}
	current := settings.Load()
	if !current.Enabled {
		return NativeAssignment{}, true, ErrNativeProvisioning
	}
	a, err = life.AdmitNativeMailbox(ctx, stateRoot, current.IssuerURL, userID, cmp.Or(mailboxID, userID), accounts)
	return a, true, err
}
