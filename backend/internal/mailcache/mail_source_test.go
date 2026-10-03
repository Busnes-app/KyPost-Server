package mailcache

import (
	"errors"
	"strings"
	"testing"
)

func TestMailSourceCacheBinding(t *testing.T) {
	source := "native:" + strings.Repeat("a", 64)
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindMailSource(source); err != nil {
		t.Fatal(err)
	}
	if err = s.Upsert("INBOX", []Entry{entry(1, "native", "unread", "body")}); err != nil {
		t.Fatal(err)
	}
	other, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"imap", "native:" + strings.Repeat("b", 64)} {
		if err = other.CheckMailSource(wrong); !errors.Is(err, ErrMailSource) {
			t.Fatal("preflight admitted wrong source", err)
		}
		if err = other.BindMailSource(wrong); !errors.Is(err, ErrMailSource) {
			t.Fatal("bound wrong source", err)
		}
	}
	if err = other.BindMailSource(source); err != nil {
		t.Fatal("binding lost in Upsert", err)
	}
	legacy := newTestStore(t)
	if _, err = legacy.Sync("INBOX", 10, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err = legacy.BindMailSource(source); !errors.Is(err, ErrMailSource) {
		t.Fatal("legacy window adopted", err)
	}
	if err = legacy.BindMailSource("imap"); err != nil {
		t.Fatal("rejection poisoned cache", err)
	}
}
