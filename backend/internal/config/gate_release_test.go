//go:build !kye2e

package config

// taggedBuild is what this test binary was built as, from the build tag alone, so a test that
// expects one build's behaviour fails if a gate file is edited.
const taggedBuild = false
