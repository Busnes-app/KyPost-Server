//go:build linux

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishDirectoryRefusesExistingTarget(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		root := t.TempDir()
		stage, target := filepath.Join(root, "stage"), filepath.Join(root, "target")
		if err := os.Mkdir(stage, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if occupied {
			if err := os.WriteFile(filepath.Join(target, "existing"), []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := PublishDirectory(stage, target); err == nil {
			t.Fatal("publication replaced an existing target")
		}
		if _, err := os.Stat(stage); err != nil {
			t.Fatal("refused publication lost staging", err)
		}
	}
	root := t.TempDir()
	stage, target := filepath.Join(root, "stage"), filepath.Join(root, "target")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "native-restore-hold.json"), []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PublishDirectory(stage, target); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(target, "native-restore-hold.json")); err != nil || string(raw) != "held" {
		t.Fatal("publication lost hold", err)
	}
}
