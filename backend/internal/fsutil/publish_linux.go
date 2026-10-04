//go:build linux

package fsutil

import "golang.org/x/sys/unix"

// PublishDirectory never replaces an existing target, even an empty directory.
func PublishDirectory(staging, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
