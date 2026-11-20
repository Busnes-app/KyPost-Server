package dbbackend

import "testing"

// SetCopyHookForTest drops or alters rows on their way into a copy until the test ends.
func SetCopyHookForTest(t testing.TB, hook func(table string, row []any) bool) {
	copyHook = hook
	t.Cleanup(func() { copyHook = nil })
}
