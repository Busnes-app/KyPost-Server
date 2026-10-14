package app

import (
	"os"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The drive reserve (5 GiB or more) would make receiving tests depend on the
// host's free space; TestNativeReceivingQuotaAndDriveReserve sets its own disk.
// Every bootstrapped store derives its admin credential (PBKDF2), so the work
// factor is lowered to the floor; no test here is about it.
func TestMain(m *testing.M) {
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 1 << 50, 1 << 50, nil }
	restore := users.SetLoginIterationsForTest(users.MinLoginIterations)
	code := m.Run()
	restore()
	os.Exit(code)
}
