package users

import (
	"context"
	"path/filepath"
	"testing"
)

// WithCurrentUsers reuses its last decode only for identical bytes: another
// process's write is seen, and a callback cannot edit the reused snapshot.
func TestCurrentUsersSnapshotIsFreshAndPrivate(t *testing.T) {
	ctx := context.Background()
	s, path := newCacheTestStore(t)
	other, err := OpenExisting(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		t.Helper()
		if err := s.WithCurrentUsers(ctx, func(all []User) error {
			n = len(all)
			all[0].Username = "edited-by-callback"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	if _, err := other.Create(ctx, "second", "correct horse battery staple", RoleUser); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("snapshot has %d users after another store's write, want %d", got, before+1)
	}
	if err := s.WithCurrentUsers(ctx, func(all []User) error {
		if all[0].Username == "edited-by-callback" {
			t.Fatal("a callback's edit reached the next snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
