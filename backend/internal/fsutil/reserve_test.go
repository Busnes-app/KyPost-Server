package fsutil

import (
	"errors"
	"testing"
)

func TestDriveReserve(t *testing.T) {
	for total, want := range map[uint64]uint64{0: 5 << 30, 40 << 30: 5 << 30, 50 << 30: 5 << 30, 100 << 30: 10 << 30, 1 << 40: 1 << 40 / 10} {
		if got := DriveReserve(total); got != want {
			t.Fatalf("DriveReserve(%d) = %d, want %d", total, got, want)
		}
	}
	defer func(orig func(string) (uint64, uint64, error)) { DiskSpace = orig }(DiskSpace)
	for _, c := range []struct {
		free, total uint64
		need        int64
		ok          bool
	}{
		{6 << 30, 20 << 30, 1 << 30, true},      // the 5 GiB floor, exactly met
		{6<<30 - 1, 20 << 30, 1 << 30, false},   // one byte short
		{11 << 30, 100 << 30, 1 << 30, true},    // 10% of the filesystem
		{11<<30 - 1, 100 << 30, 1 << 30, false}, //
		{5 << 30, 20 << 30, -1, true},           // a negative need is none
	} {
		DiskSpace = func(string) (uint64, uint64, error) { return c.free, c.total, nil }
		if err := CheckDriveReserve("/", c.need); (err == nil) != c.ok || err != nil && !errors.Is(err, ErrDriveReserve) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	DiskSpace = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs failed") }
	if err := CheckDriveReserve("/", 0); err == nil || errors.Is(err, ErrDriveReserve) {
		t.Fatalf("unmeasurable disk: %v", err)
	}
}
