package fsutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistLoadJSONFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "f.json")
	type doc struct{ N int }

	seeded := false
	if err := LoadJSONFile(path, func(doc) { t.Fatal("apply on a missing file") }, func() error { seeded = true; return nil }); err != nil || !seeded {
		t.Fatalf("missing file: err=%v seeded=%v", err, seeded)
	}
	if err := LoadJSONFile[doc](path, func(doc) {}, nil); err != nil {
		t.Fatalf("missing file with nil onMissing: %v", err)
	}

	if err := PersistJSONFile(path, doc{N: 7}); err != nil {
		t.Fatalf("PersistJSONFile: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("persisted file mode: %v %v", info, err)
	}
	var got doc
	if err := LoadJSONFile(path, func(d doc) { got = d }, nil); err != nil || got.N != 7 {
		t.Fatalf("LoadJSONFile = %+v, %v", got, err)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadJSONFile(path, func(doc) { t.Fatal("apply on corrupt JSON") }, nil); err == nil {
		t.Fatal("corrupt JSON loaded without error")
	}
}

func TestLockFileExcludesOtherHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	release, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		second, err := LockFile(path)
		if err == nil {
			second()
		}
		done <- err
	}()
	select {
	case err := <-done:
		release()
		t.Fatalf("a second holder locked while another held the lock (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("LockFile after release: %v", err)
	}
}
