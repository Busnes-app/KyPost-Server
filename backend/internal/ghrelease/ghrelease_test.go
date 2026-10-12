package ghrelease

import "testing"

func TestIsNewer(t *testing.T) {
	for _, c := range []struct {
		latest, installed string
		want              bool
	}{
		{"0.2.1", "0.2.0", true},
		{"0.10.0", "0.9.9", true}, // numeric, not lexical
		{"1.0", "0.9.9", true},
		{"1.0.1", "1.0", true},
		{"0.2.0", "0.2.0", false},
		{"1.0", "1.0.0", false},
		{"0.1.9", "0.2.0", false},
		{" 0.3.0 ", "0.2.0", true},
		{"v0.3.0", "0.2.0", false}, // callers strip the tag prefix
		{"0.3.0-rc1", "0.2.0", false},
		{"", "0.2.0", false},
		{"0.3.0", "", false},
		{"0.3.0", "dev", false},
	} {
		if got := IsNewer(c.latest, c.installed); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.latest, c.installed, got, c.want)
		}
	}
}
