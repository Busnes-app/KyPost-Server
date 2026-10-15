package users

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	oldIssuer = "https://gitea.test"
	newIssuer = "https://gitlab.test"
)

// clearSSOIssuer rewrites a record as a pre-issuer build stored it.
func clearSSOIssuer(t *testing.T, s *Store, id string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.readFileUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Users {
		if f.Users[i].ID == id {
			f.Users[i].SSOIssuer = ""
		}
	}
	if err := s.writeFileUnlocked(f); err != nil {
		t.Fatal(err)
	}
}

// A link stored without an issuer named "1" at some provider; another provider's
// "1" is a different person.
func TestUnboundSSOLinkMatchesNoIssuer(t *testing.T) {
	s := newTestStore(t)
	u, err := s.Create(context.Background(), "victim", "victim-password-123", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkSSO(u.ID, "1", "victim", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBySSOSubIssuer(newIssuer, "1"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatalf("unbound link resolved for an arbitrary issuer: %v", err)
	}
}

func TestBindUnboundSSOLinks(t *testing.T) {
	s := newTestStore(t)
	legacy, err := s.CreateSSOUser("legacy", RoleUser, "1", "legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.CreateSSOUserIssuer(newIssuer, "bound", RoleUser, "2", "bound", "")
	if err != nil {
		t.Fatal(err)
	}
	native, err := s.PublishPreparedSSOUser(context.Background(), "native-id", "native", RoleUser, oldIssuer, "3", "native", "",
		func() (string, error) { return "native:" + strings.Repeat("ab", 32), nil })
	if err != nil {
		t.Fatal(err)
	}
	clearSSOIssuer(t, s, native.ID)

	n, err := s.BindUnboundSSOLinks(newIssuer)
	if err != nil || n != 2 {
		t.Fatalf("BindUnboundSSOLinks = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]string{legacy.ID: newIssuer, bound.ID: newIssuer, native.ID: oldIssuer} {
		if u, _ := s.Get(id); u.SSOIssuer != want {
			t.Errorf("%s bound to %q, want %q", u.Username, u.SSOIssuer, want)
		}
	}
	if n, err = s.BindUnboundSSOLinks(newIssuer); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v; want 0", n, err)
	}
	if _, err := s.GetBySSOSubIssuer(newIssuer, "1"); err != nil {
		t.Fatalf("bound legacy link refused under its issuer: %v", err)
	}
}

func TestRevokeSSOLinksNotFrom(t *testing.T) {
	s := newTestStore(t)
	old, err := s.Create(context.Background(), "old", "old-password-1234", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkSSOIssuer(oldIssuer, old.ID, "1", "old", ""); err != nil {
		t.Fatal(err)
	}
	unbound, err := s.CreateSSOUser("unbound", RoleUser, "2", "unbound", "")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.CreateSSOUserIssuer(newIssuer, "current", RoleUser, "3", "current", "")
	if err != nil {
		t.Fatal(err)
	}
	native, err := s.PublishPreparedSSOUser(context.Background(), "native-id", "native", RoleUser, oldIssuer, "4", "native", "",
		func() (string, error) { return "native:" + strings.Repeat("cd", 32), nil })
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.RevokeSSOLinksNotFrom(newIssuer)
	if err != nil || n != 2 {
		t.Fatalf("RevokeSSOLinksNotFrom = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]bool{old.ID: true, unbound.ID: true, current.ID: false, native.ID: false} {
		if u, _ := s.Get(id); u.SSOLinkRevoked() != want {
			t.Errorf("%s revoked = %v, want %v", u.Username, u.SSOLinkRevoked(), want)
		}
	}
	if n, err = s.RevokeSSOLinksNotFrom(newIssuer); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v; want 0", n, err)
	}

	// A revoked link is re-authorized under the new issuer by the link flow.
	if err := s.LinkSSOIssuer(newIssuer, old.ID, "7", "old", ""); err != nil {
		t.Fatalf("relink under the new issuer: %v", err)
	}
	if u, err := s.GetBySSOSubIssuer(newIssuer, "7"); err != nil || u.ID != old.ID || u.SSOLinkRevoked() {
		t.Fatalf("relinked account = %+v, %v", u, err)
	}
}
