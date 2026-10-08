package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// NativeMailEnabled is an explicit operator opt-in. A typo cannot select IMAP
// or silently enable a native mailbox stack in only one process.
func NativeMailEnabled() (bool, error) {
	return nativeFlag("KYPOST_NATIVE_MAIL")
}

func NativeReceivingEnabled() (bool, error) {
	return nativeFlag("KYPOST_NATIVE_RECEIVING")
}

// NativeRestoreReleaseEnabled gates the future hold release; off by default.
func NativeRestoreReleaseEnabled() (bool, error) {
	return nativeFlag("KYPOST_NATIVE_RESTORE_RELEASE")
}

func nativeFlag(name string) (bool, error) {
	switch strings.TrimSpace(os.Getenv(name)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New(name + " must be true or false")
	}
}

// Mailbox quota bounds: the floor fits ten 25 MiB messages, the ceiling is 1 TiB.
const (
	MinMailboxQuotaBytes = 256 << 20
	MaxMailboxQuotaBytes = 1 << 40
)

// MailboxQuotaBytes is every native mailbox's storage quota, from
// KYPOST_MAILBOX_QUOTA_BYTES (default 5 GiB, 5 × 2^30 bytes). Every process
// reads the same value; migrate-native applies a change at the next start.
func MailboxQuotaBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("KYPOST_MAILBOX_QUOTA_BYTES"))
	if raw == "" {
		return 5 << 30, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < MinMailboxQuotaBytes || n > MaxMailboxQuotaBytes {
		return 0, errors.New("KYPOST_MAILBOX_QUOTA_BYTES must be a whole number of bytes from 268435456 (256 MiB) to 1099511627776 (1 TiB)")
	}
	return n, nil
}
