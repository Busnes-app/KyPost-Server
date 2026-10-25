//go:build hpkegen

package cryptutil_test

import (
	"os"
	"testing"
)

// Rewrites testdata/device-envelope-hpke.json; the untagged reproducibility
// test proves a rerun is byte-identical.
func TestGenerateHPKEFixture(t *testing.T) {
	if err := os.WriteFile(hpkeFixturePath, buildHPKEFixture(t), 0o644); err != nil {
		t.Fatal(err)
	}
}
