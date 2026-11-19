package cryptutil

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const raceDirEnv = "KYPOST_TEST_KEY_RACE_DIR"

var raceKeys = []string{"totp-secret.key", "imap-config.key", "pgp-private-key.key", "pickup-store.key", "pairing.key",
	"pow.key", "relay-identity.key", "scheduled-send.key", "native-relay.key", "bulk-backup.key"}

// raceCreate creates every key from n goroutines released together and returns what each
// saw, base64, one line per goroutine per key.
func raceCreate(t testing.TB, dir string, n int) []string {
	start := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			<-start
			for _, name := range raceKeys {
				k, err := LoadOrCreateKey(filepath.Join(dir, name))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen = append(seen, name+"="+base64.StdEncoding.EncodeToString(k))
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	return seen
}

// The helper process: ten goroutines on the parent's directory.
func TestKeyRaceHelperProcess(t *testing.T) {
	dir := os.Getenv(raceDirEnv)
	if dir == "" {
		t.Skip("helper process")
	}
	fmt.Println(strings.Join(raceCreate(t, dir, 10), "\n"))
}

// Twenty goroutines across two processes on one empty SECRET_DIR end with one key per file,
// the one every caller was given, and no partial or temporary file.
func TestConcurrentKeyCreationAgreesAcrossProcesses(t *testing.T) {
	for round := range 5 {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestKeyRaceHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), raceDirEnv+"="+dir)
		var out strings.Builder
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		seen := raceCreate(t, dir, 10)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v\n%s", err, out.String())
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.Contains(line, ".key=") {
				seen = append(seen, line)
			}
		}
		if len(seen) != 20*len(raceKeys) {
			t.Fatalf("round %d: %d results, want %d", round, len(seen), 20*len(raceKeys))
		}
		for _, line := range seen {
			name, got, _ := strings.Cut(line, "=")
			onDisk, err := LoadKey(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got != base64.StdEncoding.EncodeToString(onDisk) {
				t.Fatalf("round %d: a caller of %s holds a key that is not the one on disk", round, name)
			}
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != len(raceKeys) {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("round %d: directory holds %v, want only the keys", round, names)
		}
	}
}
