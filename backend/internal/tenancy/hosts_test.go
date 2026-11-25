package tenancy

import (
	"errors"
	"strings"
	"testing"
)

// The assertion refuses while any store is still written to the host's disk, naming each, and
// admits only resolved targets and per-host stores a pattern names.
func TestAssertVolumeFree(t *testing.T) {
	err := AssertVolumeFree(Targets)
	for name, target := range Targets {
		if target == TargetFile && (!errors.Is(err, ErrHostFiles) || !strings.Contains(err.Error(), name)) {
			t.Errorf("%s still on disk, not named: %v", name, err)
		}
	}
	if err := AssertVolumeFree(map[string]Target{"contacts.json": TargetDocuments, "state.db": TargetDatabase, "pow.key": TargetKey, "quota-": TargetFence, "backups": TargetAbsent, "imports": TargetHost}); err != nil {
		t.Fatal("volume-free stores:", err)
	}
	for name, target := range map[string]Target{"contacts.json": TargetFile, "scratch.json": TargetHost, "x": ""} {
		if err := AssertVolumeFree(map[string]Target{name: target}); !errors.Is(err, ErrHostFiles) {
			t.Errorf("%s as %q: %v", name, target, err)
		}
	}
}
