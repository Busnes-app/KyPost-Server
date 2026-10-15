package sso

import (
	"os"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// The drive reserve (5 GiB or more) would make mailbox writes in these tests
// depend on the host's free space; tests that check it set their own disk.
// Every user store derives its admin credential (PBKDF2); no test here is
// about the work factor, so it is lowered.
func TestMain(m *testing.M) {
	fsutil.DiskSpace = func(string) (uint64, uint64, error) { return 1 << 50, 1 << 50, nil }
	restore := users.SetLoginIterationsForTest(1000)
	code := m.Run()
	restore()
	os.Exit(code)
}
