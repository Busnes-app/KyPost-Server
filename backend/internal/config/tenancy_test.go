package config

import (
	"strings"
	"testing"
)

func TestTenancy(t *testing.T) {
	const staff = "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d"
	for _, c := range []struct {
		mode, staff, want, wantStaff string
		ok                           bool
	}{
		{"", "", TenancySingle, "", true},
		{"single", "", TenancySingle, "", true},
		{" single ", "", TenancySingle, "", true},
		{"multi", staff, TenancyMulti, staff, true},
		{"single", staff, "", "", false}, // a staff org outside multi mode
		{"", staff, "", "", false},       // the same, mode unset
		{"multi", "", "", "", false},     // multi needs the staff org
		{"multi", strings.ToUpper(staff), "", "", false},
		{"multi", "{" + staff + "}", "", "", false},
		{"multi", staff + "0", "", "", false},
		{"multi", strings.Repeat("a", 1<<16), "", "", false},
		{"Multi", staff, "", "", false},
		{"saas", "", "", "", false},
		{strings.Repeat("m", 1<<16), "", "", "", false},
	} {
		t.Setenv("KYPOST_TENANCY", c.mode)
		t.Setenv("KYPOST_STAFF_ORG_ID", c.staff)
		mode, gotStaff, err := Tenancy()
		if (err == nil) != c.ok || mode != c.want || gotStaff != c.wantStaff {
			t.Errorf("%.20q %.40q: %q %q %v", c.mode, c.staff, mode, gotStaff, err)
		}
		if err != nil && c.staff != "" && strings.Contains(err.Error(), c.staff[:8]) {
			t.Errorf("the refusal echoes the staff org: %v", err)
		}
	}
}

// Until hosted mail lands, every process refuses multi mode at startup.
func TestCheckEnvRefusesMulti(t *testing.T) {
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	if err := CheckEnv(); err == nil || !strings.Contains(err.Error(), ErrMultiTenancyUnavailable.Error()) {
		t.Fatalf("multi: %v", err)
	}
	t.Setenv("KYPOST_TENANCY", "bogus")
	if err := CheckEnv(); err == nil || !strings.Contains(err.Error(), "KYPOST_TENANCY") {
		t.Fatalf("bogus: %v", err)
	}
	for _, v := range []string{"", "single"} {
		t.Setenv("KYPOST_TENANCY", v)
		t.Setenv("KYPOST_STAFF_ORG_ID", "")
		if err := CheckEnv(); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
	}
}
