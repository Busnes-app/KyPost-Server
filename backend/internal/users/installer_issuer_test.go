package users

import (
	"errors"
	"testing"
)

func TestInstallerIssuerBindingWithoutNativeMailbox(t *testing.T) {
	s := newTestStore(t)
	owner, err := s.CreateSSOUserIssuer("https://identity.test", "owner-admin", RoleAdmin, "owner-sub", "owner-admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if owner.SSOIssuer != "https://identity.test" || owner.NativeMailboxSource != "" || owner.NativeMailboxIssuer != "" {
		t.Fatalf("wrong identity binding: %+v", owner)
	}
	if _, err = s.GetBySSOSubIssuer("https://other.test", "owner-sub"); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("foreign issuer resolved owner:", err)
	}
	if _, err = s.GetBySSOSubIssuer("https://identity.test", "owner-sub"); err != nil {
		t.Fatal(err)
	}
	if err = s.LinkSSOIssuer("https://other.test", owner.ID, "other-sub", "", ""); !errors.Is(err, ErrNativeAccountConflict) {
		t.Fatal("foreign issuer rebound owner:", err)
	}
}
