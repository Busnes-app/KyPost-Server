package mailbox

import (
	"context"
	"sync/atomic"
)

// ErrPoolFull refuses a write that adds bytes past the owner's org storage pool (tenancy
// design "Quotas"). It is ErrCapacity, so callers keep today's capacity answer, and its
// text is fit to show a member.
var ErrPoolFull error = poolFull{}

type poolFull struct{}

func (poolFull) Error() string {
	return "your organisation's storage is full; delete mail or files in any app to free space"
}
func (poolFull) Is(target error) bool { return target == ErrCapacity }

// PoolCheck answers whether owner's org has room for add more bytes: nil, ErrPoolFull, or
// the error that kept it from deciding.
type PoolCheck func(ctx context.Context, owner Owner, add int64) error

var poolCheck atomic.Pointer[PoolCheck]

// SetPoolCheck installs the multi-tenant pool check for every store in the process; nil
// removes it. Single-tenant installs never set it, so nothing is checked there.
// ponytail: process-wide, like the drive reserve; a per-store option is the upgrade if
// one process ever serves two installs.
func SetPoolCheck(f PoolCheck) {
	if f == nil {
		poolCheck.Store(nil)
		return
	}
	poolCheck.Store(&f)
}

// checkPool runs the installed check for n new bytes. exempt is checkReserve's: bytes
// already charged (a gateway receipt held while receiving accepted it).
func (s *Store) checkPool(ctx context.Context, n int64, exempt bool) error {
	f := poolCheck.Load()
	if f == nil || exempt || n <= 0 {
		return nil
	}
	return (*f)(ctx, s.owner, n)
}
