package config

import (
	"errors"
	"os"
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
