package mailbox

import (
	"context"
	"fmt"
	"sync/atomic"
)

// ErrSealTooLarge refuses incoming mail whose encrypted form would exceed the mailbox's
// message limit. It is ErrUnimportable: a file import skips it, and receiving keeps it in
// the buffer (quarantined) rather than store it in plaintext.
var ErrSealTooLarge = fmt.Errorf("%w: too large to store encrypted within the mailbox message limit", ErrUnimportable)

// Seal encrypts one new incoming message for owner before it is stored (E2E_PGP.md,
// "Incoming mail encryption"). It returns raw itself when owner has no usable key (the
// mail is stored as received until the backlog sweep seals it) and an error to refuse.
type Seal func(ctx context.Context, owner Owner, raw []byte) ([]byte, error)

var seal atomic.Pointer[Seal]

// SetSeal installs the incoming seal for every store in the process; nil removes it.
// Every process that writes mailboxes installs it at start (app); only tests run without.
// ponytail: process-wide, like SetPoolCheck.
func SetSeal(f Seal) {
	if f == nil {
		seal.Store(nil)
		return
	}
	seal.Store(&f)
}

// sealIncoming runs the installed seal over a received or imported message.
func (s *Store) sealIncoming(ctx context.Context, raw []byte) ([]byte, error) {
	f := seal.Load()
	if f == nil {
		return raw, nil
	}
	sealed, err := (*f)(ctx, s.owner, raw)
	if err != nil {
		return nil, err
	}
	if int64(len(sealed)) > s.limits.MessageBytes {
		return nil, ErrSealTooLarge
	}
	return sealed, nil
}
