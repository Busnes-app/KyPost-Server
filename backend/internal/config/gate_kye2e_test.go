//go:build kye2e

package config

import "testing"

// The end-to-end test build lets a complete multi-tenant environment through CheckEnv.
// Run alone: go test -tags kye2e -run '^TestE2EBuild' ./internal/config/ ./internal/app/ ./internal/sso/
func TestE2EBuildOpensMulti(t *testing.T) {
	t.Setenv("KYPOST_TENANCY", "multi")
	t.Setenv("KYPOST_STAFF_ORG_ID", "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d")
	if err := CheckEnv(); err != nil {
		t.Fatalf("test build refused multi: %v", err)
	}
}
