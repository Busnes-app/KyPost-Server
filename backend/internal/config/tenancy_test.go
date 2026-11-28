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
		t.Setenv("KY_CELLS", "")
		if c.mode == "multi" {
			t.Setenv("KY_CELLS", "test-1")
		}
		mode, gotStaff, err := Tenancy()
		if (err == nil) != c.ok || mode != c.want || gotStaff != c.wantStaff {
			t.Errorf("%.20q %.40q: %q %q %v", c.mode, c.staff, mode, gotStaff, err)
		}
		if err != nil && c.staff != "" && strings.Contains(err.Error(), c.staff[:8]) {
			t.Errorf("the refusal echoes the staff org: %v", err)
		}
	}
}

// Until hosted mail lands, every process refuses multi mode at startup in a release build;
// only the end-to-end test build lets it through (taggedBuild, from the build tag).
func TestCheckEnvRefusesMulti(t *testing.T) {
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KY_CELLS", "test-1")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	setS3(t)
	if err := CheckEnv(); taggedBuild && err != nil || !taggedBuild && (err == nil || !strings.Contains(err.Error(), ErrMultiTenancyUnavailable.Error())) {
		t.Fatalf("multi, test build %v: %v", taggedBuild, err)
	}
	t.Setenv("KYPOST_TENANCY", "bogus")
	if err := CheckEnv(); err == nil || !strings.Contains(err.Error(), "KYPOST_TENANCY") {
		t.Fatalf("bogus: %v", err)
	}
	for _, v := range []string{"", "single"} {
		t.Setenv("KYPOST_TENANCY", v)
		t.Setenv("KYPOST_STAFF_ORG_ID", "")
		t.Setenv("KY_CELLS", "")
		clearS3(t)
		if err := CheckEnv(); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
	}
}

// The end-to-end test build takes only an https issuer under the reserved .test domain.
func TestCheckTestIssuer(t *testing.T) {
	for issuer, ok := range map[string]bool{
		"https://id.e2e.test": true, "https://ID.E2E.TEST/": true, "https://id.e2e.test:8443": true,
		"": false, "http://id.e2e.test": false, "https://id.example": false, "https://test": false,
		"https://id.test.example": false, "https://id.e2e.test.": false, "https://idtest": false,
	} {
		if err := CheckTestIssuer(issuer); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", issuer, err, ok)
		}
	}
}

// The gate constant is the build tag's, and nothing else's: an edit to either gate file
// fails here in that build, as well as in every test that expects one build's behaviour.
func TestGateMatchesBuildTag(t *testing.T) {
	if MultiTenantBuild != taggedBuild {
		t.Fatalf("MultiTenantBuild is %v in a build tagged %v", MultiTenantBuild, taggedBuild)
	}
}

// KY_CELLS (MH9): multi mode requires 1 to 32 KyIdentity home region slugs, sorted and
// never repeated; single mode refuses it. A refusal never echoes the value.
func TestCells(t *testing.T) {
	for raw, want := range map[string]string{
		"us-east-1": "us-east-1", " us-east-2 , us-east-1 ": "us-east-1,us-east-2", "e2e-1,e2e-2,": "e2e-1,e2e-2",
		"": "", ",": "", "us-east-1,us-east-1": "", "US-EAST-1": "", "1us": "", "u": "", "us_east": "",
		strings.Repeat("a", 33): "", "us-east-1;eu-1": "",
	} {
		cells, err := Cells(raw)
		if got := strings.Join(cells, ","); got != want || (err == nil) != (want != "") {
			t.Errorf("%q: %q %v, want %q", raw, got, err, want)
		}
		if v := strings.TrimSpace(raw); err != nil && len(v) > 3 && strings.Contains(err.Error(), v) {
			t.Errorf("%q: the refusal echoes the value: %v", raw, err)
		}
	}
	many := make([]string, MaxCells+1)
	for i := range many {
		many[i] = "c-" + strings.Repeat("x", i/26+1) + string(rune('a'+i%26))
	}
	if _, err := Cells(strings.Join(many[:MaxCells], ",")); err != nil {
		t.Fatalf("%d cells: %v", MaxCells, err)
	}
	if _, err := Cells(strings.Join(many, ",")); err == nil {
		t.Fatalf("%d cells accepted", MaxCells+1)
	}

	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	for _, bad := range []string{"", "Bad"} {
		t.Setenv("KY_CELLS", bad)
		if _, _, err := Tenancy(); err == nil || !strings.Contains(err.Error(), "KY_CELLS") {
			t.Errorf("multi with KY_CELLS=%q: %v", bad, err)
		}
		if err := CheckEnv(); err == nil || !strings.Contains(err.Error(), "KY_CELLS") {
			t.Errorf("CheckEnv, multi with KY_CELLS=%q: %v", bad, err)
		}
	}
	t.Setenv("KYPOST_TENANCY", "")
	t.Setenv("KYPOST_STAFF_ORG_ID", "")
	t.Setenv("KY_CELLS", "us-east-1")
	if _, _, err := Tenancy(); err == nil || !strings.Contains(err.Error(), "KY_CELLS") {
		t.Fatalf("single with KY_CELLS: %v", err)
	}
}
