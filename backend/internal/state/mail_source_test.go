package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailSourceAdmission(t *testing.T) {
	native := "native:" + strings.Repeat("a", 64)
	dir := filepath.Join(t.TempDir(), "fresh")
	st, err := NewNative(dir, native)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.BindMailSource(native); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"imap", "native:" + strings.Repeat("b", 64), "native:bad"} {
		if err = st.BindMailSource(source); !errors.Is(err, ErrMailSource) {
			t.Fatalf("admitted %q: %v", source, err)
		}
	}
	if _, err = NewNative(dir, native); err == nil {
		t.Fatal("existing native directory adopted")
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.BindMailSource(native); err != nil {
		t.Fatal("ordinary reopen changed binding", err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	st, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.BindMailSource(native); !errors.Is(err, ErrMailSource) {
		t.Fatal("recreated state adopted old native IDs", err)
	}
	if err = st.BindMailSource("imap"); err != nil {
		t.Fatal(err)
	}
}

func TestMailSourceEmptyLegacyIsNotFresh(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec("DELETE FROM meta WHERE key='mail_source'"); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	native := "native:" + strings.Repeat("a", 64)
	if err = st.BindMailSource(native); !errors.Is(err, ErrMailSource) {
		t.Fatal("empty legacy database adopted native", err)
	}
	if _, err = NewNative(dir, native); err == nil {
		t.Fatal("existing legacy directory adopted")
	}
}

func TestOpenNativeNeverCreatesOrAdoptsState(t *testing.T) {
	source := "native:" + strings.Repeat("a", 64)
	for _, kind := range []string{"absent-directory", "missing-database", "empty-database", "legacy", "foreign", "native"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "account")
			switch kind {
			case "missing-database", "empty-database":
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "empty-database" {
					if err := os.WriteFile(filepath.Join(dir, "state.db"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "legacy":
				st, err := New(dir)
				if err != nil {
					t.Fatal(err)
				}
				_ = st.Close()
			case "foreign", "native":
				bound := source
				if kind == "foreign" {
					bound = "native:" + strings.Repeat("b", 64)
				}
				st, err := NewNative(dir, bound)
				if err != nil {
					t.Fatal(err)
				}
				_ = st.Close()
			}
			st, err := OpenNative(dir, source)
			if kind == "native" {
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				if err := st.BindMailSource(source); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				st.Close()
				t.Fatal("unprepared state accepted")
			}
			switch kind {
			case "absent-directory":
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("created directory: %v", err)
				}
			case "missing-database":
				if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
					t.Fatalf("created database: %v", err)
				}
			case "empty-database":
				info, err := os.Stat(filepath.Join(dir, "state.db"))
				if err != nil || info.Size() != 0 {
					t.Fatalf("unbound database mutated: %v", err)
				}
			}
		})
	}
}

func TestNativeStateDatabaseURIPaths(t *testing.T) {
	source := "native:" + strings.Repeat("a", 64)
	for _, name := range []string{"state#old", "state?mode=rwc", "state%20space"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, name, "users", "one")
			if _, err := OpenNative(dir, source); err == nil {
				t.Fatal("missing state accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("URI created an unexpected path: %v %v", entries, err)
			}
			st, err := NewNative(dir, source)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetCheckpoint("kept"); err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			st, err = OpenNative(dir, source)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			got, err := st.Checkpoint()
			if err != nil || got != "kept" {
				t.Fatalf("wrong database opened: %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
