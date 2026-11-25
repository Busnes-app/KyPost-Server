package vol

import (
	"path/filepath"
	"testing"
)

// FenceRootForTest fences every Lock under root with fence until the test ends, while
// documents there stay files: existing file-backed suites then run every cross-store lock
// through real fences (MH4).
func FenceRootForTest(t testing.TB, root string, fence FenceFunc) {
	t.Helper()
	if !testing.Testing() {
		panic("vol: test hook called outside a test binary")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &mount{root: abs, fence: fence}
	mounts.Lock()
	mounts.entries = append(mounts.entries, m)
	mounts.Unlock()
	t.Cleanup(func() {
		mounts.Lock()
		defer mounts.Unlock()
		for i, entry := range mounts.entries {
			if entry == m {
				mounts.entries = append(mounts.entries[:i], mounts.entries[i+1:]...)
				return
			}
		}
	})
}
