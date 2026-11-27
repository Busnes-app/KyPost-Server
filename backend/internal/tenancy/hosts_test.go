package tenancy

import (
	"errors"
	"strings"
	"testing"
)

// All production stores resolve off this host's disk (MH7); synthetic TargetFile,
// unpatterned TargetHost, and empty targets are still refused and named.
func TestAssertVolumeFree(t *testing.T) {
	if err := AssertVolumeFree(Targets); err != nil {
		t.Fatalf("Targets still has stores on the host's disk: %v", err)
	}
	for name, target := range Targets {
		if target == TargetFile {
			t.Errorf("%s is still classified as TargetFile", name)
		}
	}
	for name, target := range map[string]Target{"contacts.json": TargetFile, "scratch.json": TargetHost, "x": ""} {
		err := AssertVolumeFree(map[string]Target{name: target})
		if !errors.Is(err, ErrHostFiles) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s as %q: %v", name, target, err)
		}
	}
}
