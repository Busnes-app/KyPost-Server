package cryptutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A provisioned directory or file creates no key; a sibling path and a released one do.
func TestProvisionedPathsCreateNoKey(t *testing.T) {
	root := t.TempDir()
	dir, file := filepath.Join(root, "private"), filepath.Join(root, "elsewhere", "pow.key")
	release := ProvisionKeys(dir, file)
	for _, p := range []string{filepath.Join(dir, "scheduled-send.key"), file} {
		if _, err := LoadOrCreateKey(p); !errors.Is(err, ErrKeyNotProvisioned) {
			t.Fatal("created a provisioned key", p, err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refusal wrote a file", p)
		}
	}
	if _, err := LoadOrCreateKey(filepath.Join(root, "private2", "a.key")); err != nil {
		t.Fatal("a sibling of a provisioned directory was refused", err)
	}
	release()
	if _, err := LoadOrCreateKey(file); err != nil {
		t.Fatal("a released path still refused", err)
	}
}
