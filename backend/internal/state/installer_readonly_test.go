package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallerReadOnlyRequiresExistingBootstrap(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("accepted absent state")
	}
	path := filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("accepted empty state")
	}
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err = ro.SetSetting("installer-probe", "must-not-write"); err == nil {
		t.Fatal("read-only connection accepted a write")
	}
	if _, err = st.Setting("installer-probe"); err == nil {
		t.Fatal("probe persisted")
	}
}
