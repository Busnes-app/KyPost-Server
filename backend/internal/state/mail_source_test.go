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
