package mailbox

import "golang.org/x/sys/unix"

// Atomic absence check also protects against ordinary state.New callers, which
// do not take the preparation lock. Never replace even an empty legacy folder.
func publishPreparedAccount(staging, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
