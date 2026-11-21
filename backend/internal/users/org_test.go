package users

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

const (
	testOrg      = "1a2b3c4d-0000-4000-8000-00000000000a"
	staffTestOrg = "1a2b3c4d-0000-4000-8000-0000000000ff"
)

// OrgID is written once, at creation (tenancy design "Identity binding"): a retried
// publication naming another org and any mutation of it are refused, with users.json
// unchanged.
func TestOrgIDIsWriteOnce(t *testing.T) {
	s := newTestStore(t)
	s.StaffOrg = staffTestOrg
	u, err := s.CreateSSOUserIssuer(newIssuer, "member", RoleUser, "sub-1", "member", "", testOrg)
	if err != nil || u.OrgID != testOrg {
		t.Fatalf("%+v %v", u, err)
	}
	prepare := func() (string, error) { return "native:" + strings.Repeat("ab", 32), nil }
	if _, err := s.PublishPreparedSSOUser(context.Background(), "native-id", "native", RoleUser, newIssuer, "sub-2", "native", "", testOrg, prepare); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"", "1a2b3c4d-0000-4000-8000-00000000000b"} {
		if _, err := s.PublishPreparedSSOUser(context.Background(), "native-id", "native", RoleUser, newIssuer, "sub-2", "native", "", org, prepare); !errors.Is(err, ErrNativeAccountConflict) {
			t.Errorf("retry with org %q: %v", org, err)
		}
	}
	if _, err := s.mutate(u.ID, func(u *User) error { u.OrgID = ""; return nil }); !errors.Is(err, ErrOrgIDWriteOnce) {
		t.Errorf("mutation cleared OrgID: %v", err)
	}
	if _, err := s.updateSSOLinks(func(u *User) bool { u.OrgID = "x"; return true }); !errors.Is(err, ErrOrgIDWriteOnce) {
		t.Errorf("link update changed OrgID: %v", err)
	}
	if err := s.UnlinkSSO(u.ID); !errors.Is(err, ErrOrgIDWriteOnce) {
		t.Errorf("unlink of an org-bound account: %v", err)
	}
	if _, err := s.SetRole(u.ID, RoleAdmin); !errors.Is(err, ErrCustomerAdmin) {
		t.Errorf("customer promoted: %v", err)
	}
	if _, err := s.CreateSSOUserIssuer(newIssuer, "boss", RoleAdmin, "sub-3", "boss", "", testOrg); !errors.Is(err, ErrCustomerAdmin) {
		t.Errorf("customer administrator created: %v", err)
	}
	if _, err := s.Create(context.Background(), "local-admin", "local-admin-password-123", RoleAdmin); !errors.Is(err, ErrCustomerAdmin) {
		t.Errorf("org-less administrator created: %v", err)
	}
	if after, _ := os.ReadFile(s.path); string(after) != string(before) {
		t.Fatal("a refused write changed users.json")
	}
	if retry, err := s.PublishPreparedSSOUser(context.Background(), "native-id", "native", RoleUser, newIssuer, "sub-2", "native", "", testOrg, prepare); err != nil || retry.ID != "native-id" || retry.OrgID != testOrg {
		t.Fatalf("same-org retry: %+v %v", retry, err)
	}
	if staff, err := s.CreateSSOUserIssuer(newIssuer, "staff", RoleAdmin, "sub-4", "staff", "", staffTestOrg); err != nil || staff.Role != RoleAdmin {
		t.Fatalf("staff administrator: %+v %v", staff, err)
	}
}

// Single mode checks none of it: no StaffOrg, no extra read, today's rules.
func TestSingleModeHasNoOrgInvariants(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create(context.Background(), "local-admin", "local-admin-password-123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateSSOUserIssuer(newIssuer, "member", RoleUser, "sub-1", "member", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(u.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkSSO(u.ID); err != nil {
		t.Fatal(err)
	}
}
