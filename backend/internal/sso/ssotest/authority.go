package ssotest

import "path/filepath"

// Native authority (the directory lifecycle and the native ledger) is stored
// by sso's JSON authority backend in these config files. Tests name them only
// through these helpers, so a storage change edits one place; sso's
// TestAuthorityFixturePaths keeps them equal to the backend's own paths.

// LifecyclePath is the lifecycle record, sso-lifecycle.json.
func LifecyclePath(configDir string) string {
	return filepath.Join(configDir, "sso-lifecycle.json")
}

// LedgerPath is the native ledger, native-provisioning.json.
func LedgerPath(configDir string) string {
	return filepath.Join(configDir, "native-provisioning.json")
}

// DirectoryLockPath is the flock file of the directory fence.
func DirectoryLockPath(configDir string) string {
	return LifecyclePath(configDir) + ".lock"
}
