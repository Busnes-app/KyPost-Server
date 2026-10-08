package fsutil

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrDriveReserve refuses a write that would eat into the drive reserve.
// Callers answer it as a temporary refusal: nothing is lost, senders retry.
var ErrDriveReserve = errors.New("the drive reserve is reached; new mail waits until space is freed")

// DiskSpace reports the bytes an unprivileged writer may still use, and the
// size, of the filesystem holding path. Tests replace it.
var DiskSpace = func(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// DriveReserve is the free space mail writes leave untouched: 10% of the
// filesystem or 5 GiB, whichever is larger.
func DriveReserve(total uint64) uint64 { return max(total/10, 5<<30) }

// CheckDriveReserve refuses writing need more bytes on path's filesystem when
// that would leave less than DriveReserve free.
func CheckDriveReserve(path string, need int64) error {
	free, total, err := DiskSpace(path)
	if err != nil {
		return fmt.Errorf("cannot measure free space; preserve mail and repair storage before retrying: %w", err)
	}
	if want := uint64(max(need, 0)) + DriveReserve(total); free < want {
		return fmt.Errorf("%w (%d MiB free, %d MiB reserved)", ErrDriveReserve, free>>20, DriveReserve(total)>>20)
	}
	return nil
}
