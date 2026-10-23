package appversion

import (
	"regexp"
	"testing"
)

func TestLineIsXYZ(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(String()) {
		t.Fatalf("VERSION must hold X.Y.Z, got %q", String())
	}
}

func TestReleaseOverridesLine(t *testing.T) {
	t.Cleanup(func() { release = "" })
	release = "1.2.3.4"
	if got := String(); got != "1.2.3.4" {
		t.Fatalf("String() = %q, want the release build's version", got)
	}
}
