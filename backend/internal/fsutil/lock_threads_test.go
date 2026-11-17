package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A waiter must not hold an OS thread: a blocking flock does, and enough
// waiters behind a busy fence took a container past its task limit, where Go
// aborts (fatal error: newosproc).
func TestLockWaitersHoldNoThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fence.json")
	release, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before := osThreads(t)
	const waiters = 64
	acquired := make(chan struct{}, waiters)
	for range waiters {
		go func() {
			unlock, err := LockFile(path)
			if err != nil {
				t.Error(err)
				return
			}
			unlock()
			acquired <- struct{}{}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	during := osThreads(t)
	release()
	for range waiters {
		<-acquired
	}
	if during-before > waiters/4 {
		t.Fatalf("%d waiters took %d more OS threads", waiters, during-before)
	}
}

func osThreads(t *testing.T) int {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.Lines(status) {
		if rest, ok := bytes.CutPrefix(line, []byte("Threads:")); ok {
			n, err := strconv.Atoi(string(bytes.TrimSpace(rest)))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("no Threads line")
	return 0
}
