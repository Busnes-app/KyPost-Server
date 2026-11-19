package sso

import "testing"

func TestPlusBase(t *testing.T) {
	for in, want := range map[string]string{
		"one@x.test":           "one@x.test",
		"one+tag@x.test":       "one@x.test",
		"one+a+b@x.test":       "one@x.test",
		"+tag@x.test":          "@x.test",
		"one@plus+domain.test": "one@plus+domain.test",
		"no-at":                "no-at",
	} {
		if got := PlusBase(in); got != want {
			t.Errorf("PlusBase(%q) = %q, want %q", in, got, want)
		}
	}
}
