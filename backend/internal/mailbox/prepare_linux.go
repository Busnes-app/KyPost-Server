package mailbox

import "github.com/Busnes-app/kypost-server/backend/internal/fsutil"

// Atomic absence check also protects against ordinary state.New callers, which
// do not take the preparation lock. Never replace even an empty legacy folder.
func publishPreparedAccount(staging, target string) error {
	return fsutil.PublishDirectory(staging, target)
}
