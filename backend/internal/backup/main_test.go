package backup

import (
	"os"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// The drive reserve (5 GiB or more) would make mailbox writes in these tests
// depend on the host's free space; tests that check it set their own disk.
func TestMain(m *testing.M) {
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 1 << 50, 1 << 50, nil }
	os.Exit(m.Run())
}
